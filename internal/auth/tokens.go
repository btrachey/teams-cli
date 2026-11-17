package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type TokenSource string

const (
	TokenTypeTeams      = "teams"
	TokenTypeSkype      = "skype"
	TokenTypeChatSvcAgg = "chatsvcagg"
)

var RuntimeTokenTypes = []string{TokenTypeSkype, TokenTypeChatSvcAgg}
var OptionalTokenTypes = []string{TokenTypeTeams}

type Token struct {
	TokenType string
	Source    TokenSource
	Location  string
	Value     string
}

const (
	TokenSourceEnv  TokenSource = "env"
	TokenSourceFile TokenSource = "file"
)

// DefaultTokenDir returns the directory the token files live in.
// FOSSTEAMS_TOKEN_DIR overrides the location (the sign-in flow reads the
// same variable).
func DefaultTokenDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("FOSSTEAMS_TOKEN_DIR")); dir != "" {
		return dir, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot retrieve user home directory")
	}

	return filepath.Join(homeDir, ".config", "fossteams"), nil
}

func EnvName(tokenType string) string {
	return "MS_TEAMS_" + strings.ToUpper(tokenType) + "_TOKEN"
}

func TokenFilePath(tokenDir, tokenType string) string {
	return filepath.Join(tokenDir, "token-"+tokenType+".jwt")
}

func ResolveToken(tokenType, tokenDir string) (Token, error) {
	envName := EnvName(tokenType)
	if tokenDir == "" {
		if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
			return Token{
				TokenType: tokenType,
				Source:    TokenSourceEnv,
				Location:  envName,
				Value:     value,
			}, nil
		}
	}

	if tokenDir == "" {
		var err error
		tokenDir, err = DefaultTokenDir()
		if err != nil {
			return Token{}, err
		}
	}

	tokenPath := TokenFilePath(tokenDir, tokenType)
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return Token{}, fmt.Errorf("unable to read %s: %v", tokenPath, err)
	}

	value := strings.TrimSpace(string(tokenBytes))
	if value == "" {
		return Token{}, fmt.Errorf("%s is empty", tokenPath)
	}

	return Token{
		TokenType: tokenType,
		Source:    TokenSourceFile,
		Location:  tokenPath,
		Value:     value,
	}, nil
}

func ApplyTokenDirToEnv(tokenDir string) error {
	if strings.TrimSpace(tokenDir) == "" {
		return nil
	}

	for _, tokenType := range RuntimeTokenTypes {
		token, err := ResolveToken(tokenType, tokenDir)
		if err != nil {
			return err
		}
		if err := os.Setenv(EnvName(tokenType), token.Value); err != nil {
			return fmt.Errorf("unable to export %s from %s: %v", EnvName(tokenType), token.Location, err)
		}
	}

	return nil
}

type JWTMetadata struct {
	Audience  string
	Subject   string
	Principal string
	ExpiresAt time.Time
	HasExpiry bool
	Claims    map[string]any
}

func ParseJWT(token string) (JWTMetadata, error) {
	segments := strings.Split(strings.TrimSpace(token), ".")
	if len(segments) < 2 {
		return JWTMetadata{}, fmt.Errorf("token does not look like a JWT")
	}

	claimsBytes, err := decodeJWTSection(segments[1])
	if err != nil {
		return JWTMetadata{}, fmt.Errorf("unable to decode JWT claims: %v", err)
	}

	claims := map[string]any{}
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return JWTMetadata{}, fmt.Errorf("unable to parse JWT claims JSON: %v", err)
	}

	meta := JWTMetadata{
		Audience:  firstStringClaim(claims, "aud"),
		Subject:   firstStringClaim(claims, "sub"),
		Principal: firstStringClaim(claims, "preferred_username", "upn", "unique_name"),
		Claims:    claims,
	}

	if expValue, ok := claims["exp"]; ok {
		expUnix, ok := numericUnixClaim(expValue)
		if !ok {
			return JWTMetadata{}, fmt.Errorf("invalid exp claim")
		}
		meta.ExpiresAt = time.Unix(expUnix, 0)
		meta.HasExpiry = true
	}

	return meta, nil
}

func decodeJWTSection(raw string) ([]byte, error) {
	if data, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
		return data, nil
	}

	padded := raw
	if rem := len(raw) % 4; rem != 0 {
		padded += strings.Repeat("=", 4-rem)
	}

	return base64.URLEncoding.DecodeString(padded)
}

func firstStringClaim(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := claims[key].(type) {
		case string:
			value = strings.TrimSpace(value)
			if value != "" {
				return value
			}
		case []any:
			for _, item := range value {
				if raw, ok := item.(string); ok && strings.TrimSpace(raw) != "" {
					return strings.TrimSpace(raw)
				}
			}
		}
	}

	return ""
}

func numericUnixClaim(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case float32:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return parsed, true
		}
	case string:
		var parsed int64
		_, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &parsed)
		if err == nil {
			return parsed, true
		}
	}

	return 0, false
}
