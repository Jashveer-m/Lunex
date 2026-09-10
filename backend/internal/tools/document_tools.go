package tools

import (
	"context"
	"fmt"
	"math"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/documents"
)

type chunkRecord struct {
	DocumentID string  `json:"document_id"`
	Filename   string  `json:"filename"`
	ChunkIndex int     `json:"chunk_index"`
	Similarity float64 `json:"similarity"`
}

type searchDocumentsInput struct {
	Query string `json:"query"`
}

// searchDocumentsTool is Phase 3's vector search, with the query chosen by the
// model rather than being the user's whole message.
//
// That difference is the tool's reason to exist. Every turn already searches
// the documents with the question as written; this lets "what did the
// resupply notes say about the generator" search for "generator fuel filter",
// which scores better against the chunk than the question does. It is held to
// the same similarity floor, so a model's query cannot talk a distant chunk
// into the context.
func searchDocumentsTool(s Services) Tool {
	return define(Tool{
		Name:        SearchDocuments,
		Description: "Search the text of the user's uploaded documents for passages about something.",
		Permission:  Read,
		Params: []Param{
			{Name: "query", Type: "string", Description: "what to look for", Required: true},
		},
		Output: object(map[string]Schema{
			"count": integer("how many passages are listed"),
			"results": listOf(object(map[string]Schema{
				"document_id": uuidField("the document the passage is from"),
				"filename":    str("its filename"),
				"chunk_index": integer("which chunk of it"),
				"similarity":  number("the cosine similarity the search scored it at"),
			}, "document_id", "filename", "chunk_index", "similarity")),
		}, "count", "results"),
	},
		func(_ context.Context, _ uuid.UUID, a Args) (searchDocumentsInput, error) {
			in := searchDocumentsInput{Query: a.String("query", "q", "search", "text", "topic")}
			if in.Query == "" {
				return in, invalid(SearchDocuments, "say what to search the documents for")
			}
			if _, err := documents.ValidateSearch(documents.SearchQuery{Query: in.Query}); err != nil {
				return in, fieldProblems(SearchDocuments, err)
			}
			return in, nil
		},
		func(ctx context.Context, userID uuid.UUID, in searchDocumentsInput) (Result, error) {
			found, err := s.Documents.Search(ctx, userID, documents.SearchQuery{
				Query: in.Query, Limit: SearchLimit, MinSimilarity: s.DocumentMinSimilarity,
			})
			if err != nil {
				return Result{}, fmt.Errorf("search documents: %w", err)
			}
			records := make([]chunkRecord, 0, len(found))
			for _, r := range found {
				records = append(records, chunkRecord{
					DocumentID: r.DocumentID.String(), Filename: r.Filename, ChunkIndex: r.ChunkIndex,
					Similarity: math.Round(r.Similarity*10000) / 10000,
				})
			}
			// The passages' text is not recorded: it is already stored, chunked,
			// against the document, and copying it into every action that
			// searched would make the audit trail a second copy of the corpus.
			return Result{Output: map[string]any{"count": len(records), "results": records}, Chunks: found}, nil
		},
		func(in searchDocumentsInput) string {
			return "Search documents for " + quoted(in.Query) + "."
		},
	)
}
