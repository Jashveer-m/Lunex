package agents

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jashveer/lifeos/backend/internal/ai"
)

// Three trials of the one decision the e2e's first step depends on.
func TestQuizRoutingTrials(t *testing.T) {
	if os.Getenv("QUIZ_ROUTE_TRIALS") == "" {
		t.Skip("off")
	}
	provider := ai.NewOllama("http://localhost:11434", "llama3.2:3b", 20*time.Minute)
	router := NewRouter(provider, standard(), quiet(), Options{Timeout: 20 * time.Minute})
	msgs := []string{
		"Make me a multiple-choice quiz from relay-handbook.txt for my Kestrel relay handbook plan.",
		"Make me a quiz from relay-handbook.txt.",
		"Quiz me on relay-handbook.txt.",
	}
	for _, m := range msgs {
		start := time.Now()
		d, err := router.Decide(context.Background(), General, m)
		t.Logf("%6.1fs tool=%-20q err=%v  <- %s", time.Since(start).Seconds(), d.Tool, err, m)
	}
}
