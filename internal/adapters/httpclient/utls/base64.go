package utls

import "encoding/base64"

// base64Std is a shim exposing base64.StdEncoding.EncodeToString under a
// short name for the CONNECT helper in utls.go.
func base64Std(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
