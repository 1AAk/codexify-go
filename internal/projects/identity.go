package projects

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const openAISessionMetaKey = "openai/session"

type Identity struct {
	Key string
}

func IdentityFromMeta(meta map[string]any) *Identity {
	if meta == nil {
		return nil
	}
	raw, ok := meta[openAISessionMetaKey].(string)
	if !ok {
		return nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	sum := sha256.Sum256([]byte("codexify-go/openai-session/v1\x00" + raw))
	return &Identity{Key: hex.EncodeToString(sum[:])}
}

func (i *Identity) Short() string {
	if i == nil || len(i.Key) < 12 {
		return ""
	}
	return i.Key[:12]
}
