package config

import (
	"strings"
	"testing"
	"time"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/chat"
)

const validSecret = "0123456789abcdef0123456789abcdef" // 32 bytes

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	t.Setenv("JWT_SECRET", validSecret)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "8080" {
		t.Fatalf("Port = %q, want 8080", cfg.Port)
	}
	if cfg.AccessTokenTTL != 15*time.Minute {
		t.Fatalf("AccessTokenTTL = %v, want 15m", cfg.AccessTokenTTL)
	}
	if cfg.RefreshTokenTTL != 30*24*time.Hour {
		t.Fatalf("RefreshTokenTTL = %v, want 720h", cfg.RefreshTokenTTL)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("JWT_SECRET", validSecret)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("err = %v, want a DATABASE_URL error", err)
	}
}

// A weak signing key must stop the process, not degrade quietly.
func TestLoadRejectsShortJWTSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	for _, secret := range []string{"", "too-short"} {
		t.Setenv("JWT_SECRET", secret)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
			t.Fatalf("secret %q: err = %v, want a JWT_SECRET error", secret, err)
		}
	}
}

func TestLoadParsesDurations(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	t.Setenv("JWT_SECRET", validSecret)
	t.Setenv("ACCESS_TOKEN_TTL", "5m")
	t.Setenv("REFRESH_TOKEN_TTL", "168h")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessTokenTTL != 5*time.Minute || cfg.RefreshTokenTTL != 168*time.Hour {
		t.Fatalf("TTLs = %v / %v", cfg.AccessTokenTTL, cfg.RefreshTokenTTL)
	}

	t.Setenv("ACCESS_TOKEN_TTL", "fifteen minutes")
	if _, err := Load(); err == nil {
		t.Fatal("an unparseable duration was accepted")
	}
}

// --- Phase 4 ---------------------------------------------------------------

func TestChatDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	t.Setenv("JWT_SECRET", validSecret)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChatModel != ai.DefaultModel {
		t.Fatalf("ChatModel = %q, want %q", cfg.ChatModel, ai.DefaultModel)
	}
	if cfg.ChatTimeout != 3*time.Minute {
		t.Fatalf("ChatTimeout = %v, want 3m", cfg.ChatTimeout)
	}
	if cfg.ChatMinSimilarity != chat.DefaultMinSimilarity {
		t.Fatalf("ChatMinSimilarity = %v, want %v", cfg.ChatMinSimilarity, chat.DefaultMinSimilarity)
	}
	// A floor of zero would let an unrelated question retrieve the nearest
	// chunk however far away it is, which is the whole failure this knob
	// exists to prevent.
	if cfg.ChatMinSimilarity <= 0 {
		t.Fatal("the retrieval floor defaults to zero")
	}
}

// A typo in a bounded knob must stop the process, not be clamped into
// something that looks deliberate.
func TestChatKnobsAreBounded(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	t.Setenv("JWT_SECRET", validSecret)

	for _, tc := range []struct{ key, value string }{
		{"CHAT_TEMPERATURE", "9"},
		{"CHAT_TEMPERATURE", "-1"},
		{"CHAT_TEMPERATURE", "warm"},
		{"CHAT_MIN_SIMILARITY", "1.5"},
		{"CHAT_MIN_SIMILARITY", "-0.2"},
		{"CHAT_MAX_TOKENS", "0"},
		{"CHAT_MAX_TOKENS", "lots"},
		{"CHAT_TIMEOUT", "three minutes"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("err = %v, want a %s error", err, tc.key)
			}
		})
	}

	t.Setenv("CHAT_TEMPERATURE", "0.7")
	t.Setenv("CHAT_MIN_SIMILARITY", "0.65")
	t.Setenv("CHAT_MAX_TOKENS", "2048")
	t.Setenv("CHAT_TIMEOUT", "90s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChatTemperature != 0.7 || cfg.ChatMinSimilarity != 0.65 ||
		cfg.ChatMaxTokens != 2048 || cfg.ChatTimeout != 90*time.Second {
		t.Fatalf("cfg = %+v, want the values from the environment", cfg)
	}
}

// The server has to outlive the longest thing a request can legitimately do,
// or a stream is cut at the socket -- where the handler can no longer report
// anything.
func TestWriteTimeoutCoversTheLongestRequest(t *testing.T) {
	for _, tc := range []struct {
		name        string
		doc, chat   time.Duration
		wantAtLeast time.Duration
	}{
		{name: "chat is longest", doc: time.Minute, chat: 5 * time.Minute, wantAtLeast: 5 * time.Minute},
		{name: "upload is longest", doc: 10 * time.Minute, chat: time.Minute, wantAtLeast: 10 * time.Minute},
		{name: "both short", doc: time.Second, chat: time.Second, wantAtLeast: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{DocumentProcessTimeout: tc.doc, ChatTimeout: tc.chat}
			if got := cfg.WriteTimeout(); got < tc.wantAtLeast {
				t.Fatalf("WriteTimeout = %v, want at least %v", got, tc.wantAtLeast)
			}
		})
	}
}
