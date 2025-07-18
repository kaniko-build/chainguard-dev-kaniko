// Copyright 2016 Google, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Package credhelper implements a Docker credential helper with special facilities
for GCR authentication.
*/
package credhelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	gauth "cloud.google.com/go/auth"
	cloudcreds "cloud.google.com/go/auth/credentials"
	"github.com/GoogleCloudPlatform/docker-credential-gcr/v2/auth"
	"github.com/GoogleCloudPlatform/docker-credential-gcr/v2/config"
	"github.com/GoogleCloudPlatform/docker-credential-gcr/v2/store"
	"github.com/GoogleCloudPlatform/docker-credential-gcr/v2/util/cmd"
	"github.com/docker/docker-credential-helpers/credentials"
	"golang.org/x/oauth2"
)

// gcrCredHelper implements a credentials.Helper interface backed by a GCR
// credential store.
type gcrCredHelper struct {
	store   store.GCRCredStore
	userCfg config.UserConfig

	// helper methods, package exposed for testing
	envToken       func() (string, error)
	gcloudSDKToken func(cmd.Command) (string, error)
	credStoreToken func(store.GCRCredStore) (string, error)

	// `gcloud` exec interface, package exposed for testing
	gcloudCmd cmd.Command
}

// NewGCRCredentialHelper returns a Docker credential helper which
// specializes in GCR's authentication schemes.
func NewGCRCredentialHelper(store store.GCRCredStore, userCfg config.UserConfig) credentials.Helper {
	return &gcrCredHelper{
		store:          store,
		userCfg:        userCfg,
		credStoreToken: tokenFromPrivateStore,
		gcloudSDKToken: tokenFromGcloudSDK,
		envToken:       tokenFromEnv,
		gcloudCmd:      &cmd.RealImpl{Command: "gcloud"},
	}
}

// Delete lists all stored credentials and associated usernames.
func (*gcrCredHelper) List() (map[string]string, error) {
	return nil, errors.New("list is unimplemented")
}

// Add adds new third-party credentials to the keychain.
func (*gcrCredHelper) Add(*credentials.Credentials) error {
	return errors.New("add is unimplemented")
}

// Delete removes third-party credentials from the store.
func (*gcrCredHelper) Delete(string) error {
	return errors.New("delete is unimplemented")
}

// Get returns the username and secret to use for a given registry server URL.
func (ch *gcrCredHelper) Get(serverURL string) (string, string, error) {
	return ch.gcrCreds()
}

func (ch *gcrCredHelper) gcrCreds() (string, string, error) {
	accessToken, err := ch.getGCRAccessToken()
	if err != nil {
		if rerr, ok := err.(*oauth2.RetrieveError); ok {
			var resp struct {
				Error        string `json:"error"`
				ErrorSubtype string `json:"error_subtype"`
			}
			if err := json.Unmarshal(rerr.Body, &resp); err == nil &&
				resp.Error == "invalid_grant" &&
				resp.ErrorSubtype == "invalid_rapt" {
				fmt.Fprintln(os.Stderr, "Reauth required; opening a browser to proceed...")
				tok, err := (&auth.GCRLoginAgent{}).PerformLogin()
				if err != nil {
					return "", "", fmt.Errorf("unable to authenticate user: %v", err)
				}
				if err = ch.store.SetGCRAuth(tok); err != nil {
					return "", "", fmt.Errorf("unable to persist access token: %v", err)
				}
				fmt.Fprintln(os.Stderr, "Reauth successful!")
				// Attempt the refresh dance again, using the new token.
				if accessToken, err := ch.getGCRAccessToken(); err != nil {
					return "", "", err
				} else {
					return config.GcrOAuth2Username, accessToken, nil
				}
			}
		}
		if err != nil {
			return "", "", helperErr("could not retrieve GCR's access token", err)
		}
	}
	return config.GcrOAuth2Username, accessToken, nil
}

// getGCRAccessToken attempts to retrieve a GCR access token from the sources
// listed by ch.tokenSources, in order.
func (ch *gcrCredHelper) getGCRAccessToken() (string, error) {
	var token string
	var err error
	tokenSources := ch.userCfg.TokenSources()
	for _, source := range tokenSources {
		switch source {
		case "env":
			token, err = ch.envToken()
		case "gcloud", "gcloud_sdk": // gcloud_sdk supported for legacy reasons
			token, err = ch.gcloudSDKToken(ch.gcloudCmd)
		case "store":
			token, err = ch.credStoreToken(ch.store)
		default:
			return "", helperErr("unknown token source: "+source, nil)
		}

		// if we successfully retrieved a token, break.
		if err == nil {
			break
		}
	}

	return token, err
}

/*
tokenFromEnv retrieves a JWT access_token from the environment.

It looks for credentials in the following places, preferring the first location found:

 1. A JSON file whose path is specified by the
    GOOGLE_APPLICATION_CREDENTIALS environment variable.
 2. A JSON file in a location known to the gcloud command-line tool.
    On Windows, this is %APPDATA%/gcloud/application_default_credentials.json.
    On other systems, $HOME/.config/gcloud/application_default_credentials.json.
 3. On Google App Engine it uses the appengine.AccessToken function.
 4. On Google Compute Engine and Google App Engine Managed VMs, it fetches
    credentials from the metadata server.
    (In this final case any provided scopes are ignored.)
*/
func tokenFromEnv() (string, error) {
	creds, err := cloudcreds.DetectDefault(&cloudcreds.DetectOptions{
		Scopes:           config.GCRScopes,
		UseSelfSignedJWT: true,
	})
	if err != nil {
		return "", helperErr("failed to detect default credentials", err)
	}

	token, err := creds.Token(context.Background())
	if err != nil {
		return "", err
	}

	if !isValidToken(token) {
		return "", helperErr("token was invalid", nil)
	}

	if token.Type != "Bearer" {
		return "", helperErr(fmt.Sprintf("expected token type \"Bearer\" but got \"%s\"", token.Type), nil)
	}

	return token.Value, nil
}

// isValidToken validates that the token is not empty, is not expired, and will
// not expire in the next 10 seconds.
//
// Previously, we used token.IsValid(), but that now returns an error if the
// token expires within 255 seconds (increased from 10s). This breaks cases like
// GKE metadata server responses, which sometimes return nearly expired tokens
// expecting them to be refreshed in the background.
// See token.IsValid(): https://github.com/googleapis/google-cloud-go/blob/auth/v0.16.2/auth/auth.go#L107
func isValidToken(t *gauth.Token) bool {
	if t == nil || t.Value == "" { // invalid token
		return false
	}
	if t.Expiry.Before(time.Now().Add(10 * time.Second)) { // expires within 10s
		return false
	}
	return true
}

// tokenFromGcloudSDK attempts to generate an access_token using the gcloud SDK.
func tokenFromGcloudSDK(gcloudCmd cmd.Command) (string, error) {
	// shelling out to gcloud is the only currently supported way of
	// obtaining the gcloud access_token
	stdout, err := gcloudCmd.Exec("config", "config-helper", "--force-auth-refresh", "--format=value(credential.access_token)")
	if err != nil {
		return "", helperErr("`gcloud config config-helper` failed", err)
	}

	token := strings.TrimSpace(string(stdout))
	if token == "" {
		return "", helperErr("`gcloud config config-helper` returned an empty access_token", nil)
	}
	return token, nil
}

func tokenFromPrivateStore(store store.GCRCredStore) (string, error) {
	gcrAuth, err := store.GetGCRAuth()
	if err != nil {
		return "", err
	}
	ts := gcrAuth.TokenSource(config.OAuthHTTPContext)
	tok, err := ts.Token()
	if err != nil {
		return "", err
	}
	if !tok.Valid() {
		return "", helperErr("token was invalid", nil)
	}

	return tok.AccessToken, nil
}

func helperErr(message string, err error) error {
	if err == nil {
		return fmt.Errorf("docker-credential-gcr/helper: %s", message)
	}
	return fmt.Errorf("docker-credential-gcr/helper: %s: %v", message, err)
}
