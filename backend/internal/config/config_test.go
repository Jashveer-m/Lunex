package config

import (
	"strings"
	"testing"
	"time"

	"github.com/jashveer/lifeos/backend/internal/agents"
	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/chat"
	"github.com/jashveer/lifeos/backend/internal/memories"
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
	if cfg.ChatTimeout != DefaultChatTimeout {
		t.Fatalf("ChatTimeout = %v, want %v", cfg.ChatTimeout, DefaultChatTimeout)
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
	// 90s used to be a valid whole-turn budget on its own. Since Phase 6 the
	// turn also carries two extractions on the same context, so a budget this
	// short has to bring them down with it; see
	// TestChatTimeoutMustLeaveRoomToGenerate.
	t.Setenv("CHAT_TIMEOUT", "90s")
	t.Setenv("MEMORY_EXTRACT_TIMEOUT", "20s")
	t.Setenv("GRAPH_EXTRACT_TIMEOUT", "20s")
	t.Setenv("AGENT_TIMEOUT", "5s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChatTemperature != 0.7 || cfg.ChatMinSimilarity != 0.65 ||
		cfg.ChatMaxTokens != 2048 || cfg.ChatTimeout != 90*time.Second {
		t.Fatalf("cfg = %+v, want the values from the environment", cfg)
	}
}

// The extractions run on the turn's own context, so raising their timeouts
// without raising the turn's does not buy slower extraction -- it buys turns
// that answer, persist, and then fail at the end-of-stream frame, reported to
// the user as an answer that could not be completed and was not saved. Both
// halves of that message would be false.
//
// Since Phase 7 the routing call counts too: it runs before the answer rather
// than after it, but on the same context and out of the same budget.
//
// The default configuration has to satisfy the rule, which is the other half
// of what this pins: it is why DefaultChatTimeout is eighteen minutes.
func TestChatTimeoutMustLeaveRoomToGenerate(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	t.Setenv("JWT_SECRET", validSecret)

	defaults, err := Load()
	if err != nil {
		t.Fatalf("the default configuration does not satisfy its own rule: %v", err)
	}
	tail := defaults.MemoryExtractTimeout + defaults.GraphExtractTimeout + defaults.AgentTimeout
	if left := defaults.ChatTimeout - tail; left < tail {
		t.Fatalf("the defaults leave %s for generation against %s of extraction", left, tail)
	}

	for _, tc := range []struct{ name, chat, mem, graph string }{
		{"the extractions eat the whole budget", "2m", "60s", "60s"},
		{"one extraction raised without the turn", "4m", "60s", "3m"},
		{"a turn budget lowered under its own tail", "1m", "60s", "60s"},
		// Room for the extractions, but not once the routing call's minute is
		// counted: the Phase 6 default budget with Phase 7's tail.
		{"the routing call not budgeted for", "5m", "60s", "60s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CHAT_TIMEOUT", tc.chat)
			t.Setenv("MEMORY_EXTRACT_TIMEOUT", tc.mem)
			t.Setenv("GRAPH_EXTRACT_TIMEOUT", tc.graph)
			_, err := Load()
			if err == nil {
				t.Fatal("a budget with no room to generate was accepted")
			}
			if !strings.Contains(err.Error(), "CHAT_TIMEOUT") {
				t.Fatalf("err = %v, want it to name CHAT_TIMEOUT", err)
			}
		})
	}

	// Lowering the extractions to match is the other way out, and it works --
	// which is what makes the rule a coherence check rather than a floor under
	// how fast an operator's model is allowed to be.
	t.Setenv("CHAT_TIMEOUT", "30s")
	t.Setenv("MEMORY_EXTRACT_TIMEOUT", "5s")
	t.Setenv("GRAPH_EXTRACT_TIMEOUT", "5s")
	t.Setenv("AGENT_TIMEOUT", "5s")
	if _, err := Load(); err != nil {
		t.Fatalf("a budget that does leave room was refused: %v", err)
	}

	// With tools off there is no routing call, and its timeout is not charged
	// against the turn.
	t.Setenv("CHAT_TIMEOUT", "5m")
	t.Setenv("MEMORY_EXTRACT_TIMEOUT", "60s")
	t.Setenv("GRAPH_EXTRACT_TIMEOUT", "60s")
	t.Setenv("AGENT_TIMEOUT", "60s")
	t.Setenv("AGENT_TOOLS", "false")
	if _, err := Load(); err != nil {
		t.Fatalf("a budget that fits without the routing call was refused with tools off: %v", err)
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

// --- Phase 5 ---------------------------------------------------------------

func TestMemoryDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	t.Setenv("JWT_SECRET", validSecret)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Extraction is on by default: an assistant that never learns anything is
	// the Phase 4 assistant, and this phase is the one that changes that.
	if !cfg.MemoryExtraction {
		t.Fatal("memory extraction defaults to off")
	}
	if cfg.MemoryModel != "" {
		t.Fatalf("MemoryModel = %q, want empty -- the chat model extracts unless told otherwise", cfg.MemoryModel)
	}
	if cfg.MemoryMinSimilarity != memories.DefaultMinSimilarity {
		t.Fatalf("MemoryMinSimilarity = %v, want %v", cfg.MemoryMinSimilarity, memories.DefaultMinSimilarity)
	}
	// The floors are tuned separately, and the memory one is the higher of the
	// two; see memories.DefaultMinSimilarity.
	if cfg.MemoryMinSimilarity <= cfg.ChatMinSimilarity {
		t.Fatalf("the memory floor (%v) is not above the document floor (%v)",
			cfg.MemoryMinSimilarity, cfg.ChatMinSimilarity)
	}
	// Extraction has to finish inside the turn it runs in.
	if cfg.MemoryExtractTimeout >= cfg.ChatTimeout {
		t.Fatalf("MemoryExtractTimeout (%v) does not fit inside ChatTimeout (%v)",
			cfg.MemoryExtractTimeout, cfg.ChatTimeout)
	}
}

func TestMemoryKnobsAreBounded(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	t.Setenv("JWT_SECRET", validSecret)

	for _, tc := range []struct{ key, value string }{
		{"MEMORY_MIN_SIMILARITY", "1.5"},
		{"MEMORY_MIN_SIMILARITY", "-0.2"},
		{"MEMORY_MIN_SIMILARITY", "close enough"},
		{"MEMORY_TEMPERATURE", "9"},
		{"MEMORY_MAX_TOKENS", "0"},
		{"MEMORY_EXTRACT_TIMEOUT", "a minute"},
		// The one that would otherwise fail silently: a value that is not a
		// boolean must not quietly switch the feature off.
		{"MEMORY_EXTRACTION", "no"},
		{"MEMORY_EXTRACTION", "off"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("err = %v, want a %s error", err, tc.key)
			}
		})
	}

	t.Setenv("MEMORY_MIN_SIMILARITY", "0.7")
	t.Setenv("MEMORY_TEMPERATURE", "0")
	t.Setenv("MEMORY_MAX_TOKENS", "256")
	t.Setenv("MEMORY_EXTRACT_TIMEOUT", "20s")
	t.Setenv("MEMORY_EXTRACTION", "false")
	t.Setenv("MEMORY_MODEL", "qwen2.5:1.5b")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryMinSimilarity != 0.7 || cfg.MemoryMaxTokens != 256 ||
		cfg.MemoryExtractTimeout != 20*time.Second || cfg.MemoryExtraction ||
		cfg.MemoryModel != "qwen2.5:1.5b" {
		t.Fatalf("cfg = %+v, want the values from the environment", cfg)
	}
}

// --- Phase 7 ---------------------------------------------------------------

func TestAgentDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	t.Setenv("JWT_SECRET", validSecret)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Tools are on by default: this is the phase that adds them, and every
	// write they propose still waits for the user.
	if !cfg.AgentTools {
		t.Fatal("agent tools default to off")
	}
	if cfg.AgentModel != "" {
		t.Fatalf("AgentModel = %q, want empty -- the chat model routes unless told otherwise", cfg.AgentModel)
	}
	if cfg.AgentTimeout != agents.DefaultTimeout || cfg.AgentMaxTokens != agents.DefaultMaxTokens ||
		cfg.AgentTemperature != agents.DefaultTemperature {
		t.Fatalf("cfg = %+v, want the agents package defaults", cfg)
	}
}

func TestAgentKnobsAreBounded(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/lifeos")
	t.Setenv("JWT_SECRET", validSecret)

	for _, tc := range []struct{ key, value string }{
		{"AGENT_TOOLS", "no"},
		{"AGENT_TOOLS", "maybe"},
		{"AGENT_TIMEOUT", "a minute"},
		{"AGENT_TEMPERATURE", "3"},
		{"AGENT_MAX_TOKENS", "0"},
		{"AGENT_MAX_TOKENS", "few"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("err = %v, want a %s error", err, tc.key)
			}
		})
	}

	t.Setenv("AGENT_TOOLS", "false")
	t.Setenv("AGENT_MODEL", "qwen2.5:1.5b")
	t.Setenv("AGENT_TIMEOUT", "20s")
	t.Setenv("AGENT_TEMPERATURE", "0")
	t.Setenv("AGENT_MAX_TOKENS", "128")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentTools || cfg.AgentModel != "qwen2.5:1.5b" || cfg.AgentTimeout != 20*time.Second ||
		cfg.AgentMaxTokens != 128 {
		t.Fatalf("cfg = %+v, want the values from the environment", cfg)
	}
}
