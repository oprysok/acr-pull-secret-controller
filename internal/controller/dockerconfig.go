package controller

import (
	"encoding/base64"
	"encoding/json"

	"github.com/oprysok/acr-pull-secret-controller/internal/token"
)

// acrTokenUser is the fixed user name ACR expects when the password is a refresh token.
const acrTokenUser = "00000000-0000-0000-0000-000000000000"

type dockerAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

// DockerConfigJSON renders a .dockerconfigjson document with one entry per registry.
// Output is deterministic (map keys are sorted), so unchanged credentials produce identical bytes.
func DockerConfigJSON(creds []token.Credential) ([]byte, error) {
	auths := make(map[string]dockerAuth, len(creds))
	for _, c := range creds {
		auths[c.Registry] = dockerAuth{
			Username: acrTokenUser,
			Password: c.RefreshToken,
			Auth:     base64.StdEncoding.EncodeToString([]byte(acrTokenUser + ":" + c.RefreshToken)),
		}
	}
	return json.Marshal(map[string]any{"auths": auths})
}
