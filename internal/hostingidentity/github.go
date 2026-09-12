package hostingidentity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func (session *Session) fetchGitHub(ctx context.Context) (Credential, time.Time, error) {
	jwt, err := session.githubJWT()
	if err != nil {
		return Credential{}, time.Time{}, err
	}
	session.secrets.remember(jwt)
	var app struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	}
	if err := session.requestJSON(ctx, "resolve GitHub App", http.MethodGet, "/app", "Bearer "+jwt, nil, &app); err != nil {
		return Credential{}, time.Time{}, err
	}
	if app.ID != session.resolved.Definition.AppID || !validPathSegment(app.Slug) {
		return Credential{}, time.Time{}, session.failure("resolve GitHub App", "hosting server returned a different or invalid app")
	}
	var token struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	repositoryName := strings.Split(session.resolved.Target.Repo, "/")[1]
	payload := struct {
		Repositories []string `json:"repositories"`
	}{[]string{repositoryName}}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", session.resolved.Definition.InstallationID)
	if err := session.requestJSON(ctx, "create GitHub installation token", http.MethodPost, path, "Bearer "+jwt, payload, &token); err != nil {
		return Credential{}, time.Time{}, err
	}
	session.secrets.remember(token.Token)
	if !validToken(token.Token) || !token.ExpiresAt.After(session.manager.now()) {
		return Credential{}, time.Time{}, session.failure("create GitHub installation token", "hosting server returned an empty, invalid, or expired token")
	}
	if err := session.verifyRepository(ctx, "Bearer "+token.Token); err != nil {
		return Credential{}, time.Time{}, err
	}
	login := app.Slug + "[bot]"
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Email string `json:"email"`
	}
	if err := session.requestJSON(ctx, "resolve GitHub bot account", http.MethodGet, "/users/"+url.PathEscape(login), "Bearer "+token.Token, nil, &user); err != nil {
		return Credential{}, time.Time{}, err
	}
	if user.ID <= 0 || !strings.EqualFold(user.Login, login) {
		return Credential{}, time.Time{}, session.failure("resolve GitHub bot account", "hosting server returned a different or invalid bot account")
	}
	email := user.Email
	if session.apiURL == "https://api.github.com" {
		email = strconv.FormatInt(user.ID, 10) + "+" + login + "@users.noreply.github.com"
	}
	credential, err := session.attribution(Credential{Token: token.Token, Login: login, NumericID: user.ID, Name: login, Email: email, ExpiresAt: token.ExpiresAt})
	if err != nil {
		return Credential{}, time.Time{}, err
	}
	// Refresh early, while also permitting servers with short token lifetimes.
	now := session.manager.now()
	lifetime := token.ExpiresAt.Sub(now)
	if lifetime <= 0 {
		return Credential{}, time.Time{}, session.failure("create GitHub installation token", "token expired before authentication completed")
	}
	margin := 5 * time.Minute
	if lifetime <= 2*margin {
		margin = lifetime / 2
	}
	return credential, token.ExpiresAt.Add(-margin), nil
}

func (session *Session) githubJWT() (string, error) {
	definition := session.resolved.Definition
	data, err := session.manager.readFile(definition.PrivateKeyFile)
	if err != nil {
		return "", session.failure("read GitHub App private key", "cannot read configured private-key file")
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", session.failure("read GitHub App private key", "configured private key must be RSA PEM")
	}
	var privateKey *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		privateKey, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var key any
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			privateKey, _ = key.(*rsa.PrivateKey)
		}
	default:
		return "", session.failure("read GitHub App private key", "configured private key must be unencrypted RSA PEM")
	}
	if err != nil || privateKey == nil || privateKey.Validate() != nil {
		return "", session.failure("read GitHub App private key", "configured private key is not valid RSA PEM")
	}
	now := session.manager.now()
	claims, _ := json.Marshal(struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}{now.Add(-time.Minute).Unix(), now.Add(9 * time.Minute).Unix(), strconv.FormatInt(definition.AppID, 10)})
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(payload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", session.failure("sign GitHub App JWT", "cannot sign app authentication request")
	}
	return payload + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func validToken(token string) bool {
	return token != "" && len(token) <= maxResponseBytes && !strings.ContainsFunc(token, unicode.IsSpace) && !strings.ContainsFunc(token, unicode.IsControl)
}
