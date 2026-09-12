package hostingidentity

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func (session *Session) forgejoToken() (string, error) {
	value, ok := session.manager.lookupEnv(session.resolved.Definition.TokenEnv)
	if !ok || strings.TrimSpace(value) == "" {
		return "", session.failure("read Forgejo token", "configured token environment variable is unset or empty")
	}
	session.secrets.remember(value)
	token := strings.TrimSpace(value)
	session.secrets.remember(token)
	if !validToken(token) {
		return "", session.failure("read Forgejo token", "configured token environment variable contains an invalid token")
	}
	return token, nil
}

func (session *Session) fetchForgejo(ctx context.Context, token string) (Credential, time.Time, error) {
	var account struct {
		ID       int64  `json:"id"`
		Login    string `json:"login"`
		FullName string `json:"full_name"`
		Email    string `json:"email"`
	}
	if err := session.requestJSON(ctx, "resolve Forgejo account", http.MethodGet, "/user", "token "+token, nil, &account); err != nil {
		return Credential{}, time.Time{}, err
	}
	if account.ID <= 0 || !validPathSegment(account.Login) {
		return Credential{}, time.Time{}, session.failure("resolve Forgejo account", "hosting server returned an invalid account")
	}
	if err := session.verifyRepository(ctx, "token "+token); err != nil {
		return Credential{}, time.Time{}, err
	}
	name := account.FullName
	if strings.TrimSpace(name) == "" {
		name = account.Login
	}
	credential, err := session.attribution(Credential{Token: token, Login: account.Login, NumericID: account.ID, Name: name, Email: account.Email})
	if err != nil {
		return Credential{}, time.Time{}, err
	}
	// Forgejo PATs do not return an expiry. Revalidate periodically, and on
	// every environment-token change or explicit invalidation after rejection.
	return credential, session.manager.now().Add(5 * time.Minute), nil
}
