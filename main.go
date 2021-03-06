package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"bufio"
	"math/rand"

	"github.com/coreos/go-oidc"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/oauth2"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Cluster structure for configuration
type Cluster struct {
	ClientID     string
	ClientSecret string
	Issuer       string
	Server       string
	ExtraScopes  []string

	// Skip verification of the ssl certificate in the kube config
	InsecureSkipTLSVerify bool

	// Does the provider use "offline_access" scope to request a refresh token
	// or does it use "access_type=offline" (e.g. Google)?
	OfflineAccess bool
}

// Claim we need to reference
type Claim struct {
	Expiration int64 `json:"exp"`
}

type app struct {
	Cluster

	name        string
	provider    *oidc.Provider
	force       bool
	skipBrowser bool
}

var (
	version    = "dev"
	commit     = "none"
	date       = "unknown"
	configFile = ".k8s-auth"
)

var a app
var config map[string]Cluster
var rootCmd = &cobra.Command{
	Use:   "k8s-auth",
	Short: "A OpenID client for out of band authorization.",
	Long:  "k8s-auth NAME",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		a.name = args[0]

		err := viper.Unmarshal(&config)
		if err != nil {
			return err
		}

		if val, ok := config[a.name]; ok {
			a.Cluster = val
		}

		if a.Issuer == "" {
			a.Issuer = "http://127.0.0.1:5556"
		}

		if a.ClientID == "" {
			a.ClientID = "kubernetes"
		}

		if a.ClientSecret == "" {
			a.ClientSecret = "cli-secret"
		}

		if !a.force && a.checkAuth() {
			switchKubeContext(a.name)
			return nil
		}

		ctx := oidc.ClientContext(context.Background(), &http.Client{})
		a.provider, err = oidc.NewProvider(ctx, a.Issuer)
		if err != nil {
			return fmt.Errorf("failed to query provider %q: %v", a.Issuer, err)
		}

		a.login()
		code, err := a.readCode()
		if err != nil {
			return err
		}

		token, refresh, err := a.fetchToken(code)
		if err != nil {
			return err
		}
		return a.applyAuth(token, refresh)
	},
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().BoolVar(&a.force, "force", false, "force update new credentials")
	rootCmd.PersistentFlags().BoolVar(&a.skipBrowser, "skip-browser", false, "skip opening the web browser")
}

func initConfig() {
	viper.SetConfigName(configFile)
	viper.AddConfigPath("$HOME")
	viper.AddConfigPath(".")
	viper.SetEnvPrefix("K8S_AUTH")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil {
		log.Debugf("Using config file:", viper.ConfigFileUsed())
	} else {
		log.Errorf("%v", err)
	}
}

func (a *app) offlineSupported() (bool, error) {
	var s struct {
		// What scopes does a provider support?
		//
		// See: https://openid.net/specs/openid-connect-discovery-1_0.html#ProviderMetadata
		ScopesSupported []string `json:"scopes_supported"`
	}
	if err := a.provider.Claims(&s); err != nil {
		return false, fmt.Errorf("failed to parse provider scopes_supported: %v", err)
	}

	if len(s.ScopesSupported) == 0 {
		// scopes_supported is a "RECOMMENDED" discovery claim, not a required
		// one. If missing, assume that the provider follows the spec and has
		// an "offline_access" scope.
		return true, nil
	}

	// See if scopes_supported has the "offline_access" scope.
	for _, scope := range s.ScopesSupported {
		if scope == oidc.ScopeOfflineAccess {
			return true, nil
		}
	}
	return false, nil
}

func (a *app) oauth2Config(scopes []string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     a.ClientID,
		ClientSecret: a.ClientSecret,
		Endpoint:     a.provider.Endpoint(),
		Scopes:       scopes,
		RedirectURL:  "urn:ietf:wg:oauth:2.0:oob",
	}
}

func (a *app) login() {
	state := randomString(34)
	scopes := append(a.ExtraScopes, "openid", "profile", "email", "groups")

	offline, err := a.offlineSupported()
	if err != nil {
		log.Infof("error processing offline support %v", err)
	}

	var url string
	if !a.OfflineAccess {
		url = a.oauth2Config(scopes).AuthCodeURL(state)
	} else if offline {
		scopes = append(scopes, "offline_access")
		url = a.oauth2Config(scopes).AuthCodeURL(state)
	} else {
		url = a.oauth2Config(scopes).AuthCodeURL(state, oauth2.AccessTypeOffline)
	}

	openBrowser(url, a.skipBrowser)
}

func (a *app) readCode() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	log.Info("Enter the code returned to you: ")
	code, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	code = strings.TrimSpace(code)
	return code, nil
}

func (a *app) fetchToken(code string) (string, string, error) {
	var (
		err   error
		token *oauth2.Token
	)

	ctx := oidc.ClientContext(context.Background(), &http.Client{})
	oauth2Config := a.oauth2Config(nil)
	token, err = oauth2Config.Exchange(ctx, code)
	if err != nil {
		return "", "", err
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return "", "", errors.New("no id_token in token response")
	}

	verifier := a.provider.Verifier(&oidc.Config{ClientID: a.ClientID})
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return "", "", err
	}

	var claims json.RawMessage
	idToken.Claims(&claims)

	buff := new(bytes.Buffer)
	json.Indent(buff, []byte(claims), "", "  ")

	data := buff.Bytes()

	file, err := cacheFile(a.name)
	if err != nil {
		return "", "", err
	}

	err = ioutil.WriteFile(file, data, 0644)
	if err != nil {
		return "", "", err
	}

	return rawIDToken, token.RefreshToken, nil
}

func (a *app) applyAuth(idToken, refreshToken string) error {
	config := &clientcmdapi.Config{
		CurrentContext: a.name,
		Contexts: map[string]*clientcmdapi.Context{
			a.name: &clientcmdapi.Context{
				Cluster:  a.name,
				AuthInfo: a.name,
			},
		},
		Clusters: map[string]*clientcmdapi.Cluster{
			a.name: &clientcmdapi.Cluster{
				InsecureSkipTLSVerify: a.InsecureSkipTLSVerify,
				Server:                a.Server,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			a.name: &clientcmdapi.AuthInfo{
				AuthProvider: &clientcmdapi.AuthProviderConfig{
					Name: "oidc",
					Config: map[string]string{
						"client-id":      a.ClientID,
						"client-secret":  a.ClientSecret,
						"id-token":       idToken,
						"idp-issuer-url": a.Issuer,
						"refresh-token":  refreshToken,
					},
				},
			},
		},
	}

	return updateKubeConfig(config)
}

func (a *app) checkAuth() bool {
	file, err := cacheFile(a.name)
	if err != nil {
		log.Error(err)
		return false
	}

	jsonFile, err := os.Open(file)
	if err != nil {
		return false
	}
	defer jsonFile.Close()

	b, _ := ioutil.ReadAll(jsonFile)

	var claim Claim
	err = json.Unmarshal(b, &claim)
	if err != nil {
		log.Error(err)
		return false
	}

	result := time.Unix(claim.Expiration, 0)
	if time.Now().Before(result) {

		cfg, err := loadKubeConfig()
		if err != nil {
			log.Error(err)
			return false
		}

		for k := range cfg.Contexts {
			if k == a.name {
				return true
			}
		}
	}
	return false
}

func randomString(length int) string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = chars[r.Intn(len(chars))]
	}
	return string(result)
}

func openBrowser(url string, skipBrowser bool) {
	command := ""
	var args []string

	if !skipBrowser {
		switch os := runtime.GOOS; os {
		case "darwin":
			command = "open"
		case "linux":
			command = "xdg-open"
		case "windows":
			command = "rundll32"
			args = append(args, "url.dll,FileProtocolHandler")
		default:
			log.Info("unable to detect OS")
		}
	}

	args = append(args, url)

	var err error
	if command != "" {
		cmd := exec.Command(command, args...)
		err := cmd.Start()
		if err != nil {
			log.Infof("unable to open browser %v\n", err)
		}
	}

	if err != nil || command == "" {
		log.Infof("open this url in your browser %s\n", url)
	}
}

func cacheFile(name string) (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", err
	}

	return filepath.Join(usr.HomeDir, configFile+"-"+name), nil
}

func switchKubeContext(name string) error {
	config := &clientcmdapi.Config{
		CurrentContext: name,
	}

	return updateKubeConfig(config)
}

func updateKubeConfig(config *clientcmdapi.Config) error {
	tmp, err := ioutil.TempFile("", "")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	clientcmd.WriteToFile(*config, tmp.Name())

	kubeConfigPath, err := kubeConfigPath()
	if err != nil {
		return err
	}

	loadingRules := clientcmd.ClientConfigLoadingRules{
		Precedence: []string{tmp.Name(), kubeConfigPath},
	}
	mergedConfig, err := loadingRules.Load()
	if err != nil {
		return err
	}

	return clientcmd.WriteToFile(*mergedConfig, kubeConfigPath)
}

func kubeConfigPath() (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(usr.HomeDir, ".kube", "config"), nil
}

func loadKubeConfig() (clientcmdapi.Config, error) {
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, &clientcmd.ConfigOverrides{}).RawConfig()
}
