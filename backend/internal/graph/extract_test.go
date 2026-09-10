package graph

import (
	"strings"
	"testing"
)

// The parser is the piece between a small local model and the database, so
// these cover the shapes a model actually produces rather than the one the
// prompt asked for.

func TestParseExtractionAcceptsTheShapesAModelProduces(t *testing.T) {
	want := Candidate{
		From: "the user", FromType: NodePerson, Relationship: RelStudies,
		To: "Go", ToType: NodeSkill, Confidence: 0.9,
	}

	for _, tc := range []struct{ name, reply string }{
		{"a bare array", `[{"from":"the user","from_type":"person","relationship":"STUDIES","to":"Go","to_type":"skill","confidence":0.9}]`},
		{"a fenced array", "```json\n[{\"from\":\"the user\",\"from_type\":\"person\",\"relationship\":\"STUDIES\",\"to\":\"Go\",\"to_type\":\"skill\",\"confidence\":0.9}]\n```"},
		{"prose then an array", `Here are the relationships I found:
[{"from":"the user","from_type":"person","relationship":"STUDIES","to":"Go","to_type":"skill","confidence":0.9}]`},
		{"a wrapper object", `{"relationships":[{"from":"the user","from_type":"person","relationship":"STUDIES","to":"Go","to_type":"skill","confidence":0.9}]}`},
		{"one bare object", `{"from":"the user","from_type":"person","relationship":"STUDIES","to":"Go","to_type":"skill","confidence":0.9}`},
		{"quoted scores", `[{"from":"the user","from_type":"person","relationship":"STUDIES","to":"Go","to_type":"skill","confidence":"0.9"}]`},
		{"subject/object aliases", `[{"subject":"the user","subject_type":"person","predicate":"STUDIES","object":"Go","object_type":"skill","confidence":0.9}]`},
		{"the pipe fallback", `the user | person | STUDIES | Go | skill | 0.9`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseExtraction(tc.reply)
			if len(got) != 1 {
				t.Fatalf("parsed %d relationships, want 1: %+v", len(got), got)
			}
			if got[0] != want {
				t.Fatalf("got %+v, want %+v", got[0], want)
			}
		})
	}
}

func TestParseExtractionRejectsWhatIsNotAnExtraction(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"an empty array", `[]`},
		{"nothing at all", ``},
		{"prose", `I could not find any relationships in this exchange.`},
		{"prose with a full stop", `There is nothing here. Nothing to connect.`},
		{"a triple with no object", `[{"from":"the user","relationship":"STUDIES","confidence":0.9}]`},
		{"a triple with no subject", `[{"to":"Go","relationship":"STUDIES","confidence":0.9}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseExtraction(tc.reply); got != nil {
				t.Fatalf("parsed %+v, want nothing", got)
			}
		})
	}
}

// The cap is enforced here rather than trusted to the prompt.
func TestParseExtractionCapsAndDeduplicates(t *testing.T) {
	reply := `[
	  {"from":"the user","relationship":"STUDIES","to":"Go","confidence":0.9},
	  {"from":"The User","relationship":"studies","to":"go","confidence":0.7},
	  {"from":"the user","relationship":"WORKS_ON","to":"the backend project","confidence":0.9},
	  {"from":"the user","relationship":"KNOWS","to":"Alice","confidence":0.9},
	  {"from":"the user","relationship":"KNOWS","to":"Bob","confidence":0.9}
	]`
	got := ParseExtraction(reply)
	if len(got) != MaxRelationshipsPerTurn {
		t.Fatalf("parsed %d, want the cap of %d: %+v", len(got), MaxRelationshipsPerTurn, got)
	}
	// The second entry is the first one restated in different case, so it is
	// the same relationship and must not take a slot.
	if got[1].To != "the backend project" {
		t.Fatalf("result 1 is %+v -- the case-varied duplicate was not folded", got[1])
	}
}

// An unreadable or absent confidence becomes the neutral default rather than a
// confident one. The default sits exactly on the floor, so such a relationship
// is admitted -- while one the model actually scored below the floor is not,
// which is the boundary DefaultScore's comment argues for.
func TestParseExtractionDefaultsAnUnreadableConfidence(t *testing.T) {
	got := ParseExtraction(`[{"from":"the user","relationship":"STUDIES","to":"Go","confidence":"very sure"}]`)
	if len(got) != 1 || got[0].Confidence != DefaultScore {
		t.Fatalf("got %+v, want the neutral default %v", got, DefaultScore)
	}
	const said = "I am learning Go for the backend project."
	if _, ok := ValidateCandidate(got[0], said); !ok {
		t.Fatal("an unscored relationship was dropped; the default must be admitted")
	}

	scored := ParseExtraction(`[{"from":"the user","relationship":"STUDIES","to":"Go","confidence":0.3}]`)
	if _, ok := ValidateCandidate(scored[0], said); ok {
		t.Fatal("a relationship the model scored 0.3 passed the quality gate")
	}
}

func TestParseExtractionClampsAScoreOutOfRange(t *testing.T) {
	got := ParseExtraction(`[{"from":"the user","relationship":"STUDIES","to":"Go","confidence":80}]`)
	if len(got) != 1 || got[0].Confidence != 1 {
		t.Fatalf("got %+v, want the score clamped to 1 -- the column has a CHECK constraint", got)
	}
}

// An unrecognised relationship becomes RELATED_TO rather than costing the
// edge: the pair is the finding, the label on the arrow is a facet.
func TestNormalizeRelationship(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"STUDIES", RelStudies},
		{"studies", RelStudies},
		{"depends on", RelDependsOn},
		{"works-on", RelWorksOn},
		{"  Knows  ", RelKnows},
		{"USES", RelRelatedTo},
		{"", RelRelatedTo},
		{"SPENT_ON", RelRelatedTo}, // out of scope this phase; not a reason to lose the pair
	} {
		if got := NormalizeRelationship(tc.in); got != tc.want {
			t.Fatalf("NormalizeRelationship(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A conversation cannot name a task, goal, note or document into existence:
// those nodes come from sync and carry a ref, and an extracted one claiming to
// be a task would say it mirrors a row that it does not.
func TestNormalizeNodeTypeCannotProduceAMirroredType(t *testing.T) {
	for _, in := range []string{"task", "goal", "note", "document", "", "nonsense", "language"} {
		got := NormalizeNodeType(in, NodeProject)
		if got == NodeTask || got == NodeGoal || got == NodeNote || got == NodeDocument {
			t.Fatalf("NormalizeNodeType(%q) = %q, which is a mirrored type", in, got)
		}
		found := false
		for _, ok := range ExtractedTypes {
			found = found || got == ok
		}
		if !found {
			t.Fatalf("NormalizeNodeType(%q) = %q, which is not an extracted type", in, got)
		}
	}
}

// The type fallback is taken from the relationship rather than fixed, because
// STUDIES points at something learnable and WORKS_ON points at work.
func TestNodeTypesAreInferredFromTheRelationshipWhenTheModelOmitsThem(t *testing.T) {
	got := ParseExtraction(`[{"from":"the user","relationship":"STUDIES","to":"Go","confidence":0.9},
	                         {"from":"Alice","relationship":"WORKS_ON","to":"the compiler","confidence":0.9}]`)
	if len(got) != 2 {
		t.Fatalf("parsed %d, want 2", len(got))
	}
	if got[0].FromType != NodePerson || got[0].ToType != NodeSkill {
		t.Fatalf("STUDIES inferred %s -> %s, want person -> skill", got[0].FromType, got[0].ToType)
	}
	if got[1].FromType != NodePerson || got[1].ToType != NodeProject {
		t.Fatalf("WORKS_ON inferred %s -> %s, want person -> project", got[1].FromType, got[1].ToType)
	}
}

// The measured failure this rule exists for: asked to name entities, a small
// model answers with a clause lifted out of the retrieved context.
func TestNameableRejectsASentence(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  bool
	}{
		{"Go", true},
		{"Alice", true},
		{"the backend project", true},
		{"systems programming coursework", true},
		{"The aurora borealis appeared over the tundra shortly after midnight", false},
		{"", false},
		{strings.Repeat("a", MaxLabelLen+1), false},
	} {
		if got := Nameable(tc.label); got != tc.want {
			t.Fatalf("Nameable(%.40q) = %t, want %t", tc.label, got, tc.want)
		}
	}
}

func TestValidateCandidate(t *testing.T) {
	const said = "I am learning Go for the backend project, and Priya is helping."

	ok := Candidate{From: "the user", To: "Go", Relationship: RelStudies, Confidence: 0.9}
	if _, pass := ValidateCandidate(ok, said); !pass {
		t.Fatal("a good candidate was rejected")
	}

	for _, tc := range []struct {
		name string
		c    Candidate
	}{
		{"a low confidence", Candidate{From: "the user", To: "Go", Confidence: MinKeptConfidence - 0.01}},
		{"a sentence as a name", Candidate{From: "the user", To: "Go is a language the user has been learning since March", Confidence: 0.9}},
		{"both ends the same", Candidate{From: "Go", To: "go", Confidence: 0.9}},
		{"an empty end", Candidate{From: "the user", To: "", Confidence: 0.9}},
		{"an entity the user never named", Candidate{From: "the user", To: "the tundra", Confidence: 0.9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, pass := ValidateCandidate(tc.c, said); pass {
				t.Fatalf("%+v passed the quality gate", tc.c)
			}
		})
	}
}

// The measured failure this rule exists for: handed a turn where the assistant
// quoted the user's own documents back at them, a small model returns the
// *document's* nouns as the entities. Every one of them is a perfectly good
// name; none of them is what the exchange was about.
func TestGroundedInMessageAdmitsATwoLetterName(t *testing.T) {
	const said = "I have been learning Go this term for the backend project."
	for _, label := range []string{"Go", "the backend project", "the user"} {
		if !GroundedInMessage(label, said) {
			t.Fatalf("GroundedInMessage(%q) = false; the user named it", label)
		}
	}
	if GroundedInMessage("Godot", said) {
		t.Fatal(`"Godot" was grounded by the word "Go"; the match must be on a word boundary`)
	}
}

func TestGroundedInMessageRejectsEntitiesLiftedFromTheContext(t *testing.T) {
	const said = "Something to keep in mind about me: I always study in the early morning " +
		"before class, and this term's systems programming coursework is in Rust."

	for _, tc := range []struct {
		label string
		want  bool
	}{
		// The user is always present in their own message.
		{"the user", true},
		{"I", true},
		{"You", true},
		// Named outright.
		{"Rust", true},
		// Two letters, and the entity this whole phase is named after in every
		// example: the grounding floor must not exclude it.
		{"Go", false}, // not in *this* message
		// A label of function words short enough to clear the length floor is
		// still not evidence.
		{"of my", false},
		// Named in different words: no exact substring, but the significant
		// words are all there.
		{"the systems programming coursework", true},
		{"early morning study", true},
		// Lifted out of the retrieved document -- the real extractions from a
		// real run of scripts/e2e.sh.
		{"aurora borealis", false},
		{"tundra", false},
		{"field notes from 14 March", false},
		{"field-notes.txt", false},
		// A label of nothing but stop words is not evidence of anything.
		{"the this that", false},
		{"", false},
	} {
		if got := GroundedInMessage(tc.label, said); got != tc.want {
			t.Fatalf("GroundedInMessage(%q) = %t, want %t", tc.label, got, tc.want)
		}
	}
}

// Extraction is a model call, so a turn with nothing in it must not make one.
func TestWorthExtracting(t *testing.T) {
	if WorthExtracting("hi", "Hello! How can I help?") {
		t.Fatal("a greeting is worth a model call")
	}
	long := "I am learning Go so I can finish the backend project this term, " +
		"and my flatmate Priya has been helping me with the concurrency parts."
	if !WorthExtracting(long, "Noted -- the backend project is the only thing you have due this term.") {
		t.Fatal("a substantial exchange is not worth a model call")
	}
}

// The prompt has to carry both halves of the exchange and the closed sets, or
// the parser downstream is being asked to fix a prompt problem.
func TestExtractionPromptCarriesTheExchangeAndTheAllowLists(t *testing.T) {
	msgs := ExtractionPrompt("I am learning Go.", "Noted.")
	if len(msgs) != 2 {
		t.Fatalf("prompt has %d messages, want 2", len(msgs))
	}
	whole := msgs[0].Content + "\n" + msgs[1].Content
	for _, want := range append([]string{"I am learning Go.", "Noted.", "skill", "person", "project"}, Relationships...) {
		if !strings.Contains(whole, want) {
			t.Fatalf("the extraction prompt never mentions %q", want)
		}
	}
	// The relationships this phase does not have must not be offered.
	for _, absent := range []string{"SPENT_ON", "VISITED"} {
		if strings.Contains(whole, absent) {
			t.Fatalf("the prompt offers %s, which has no node type to point at", absent)
		}
	}
}

func TestExtractionPromptTruncatesAHugeExchange(t *testing.T) {
	msgs := ExtractionPrompt(strings.Repeat("x", MaxExchangeChars*2), "ok")
	if n := len([]rune(msgs[1].Content)); n > MaxExchangeChars*2 {
		t.Fatalf("the prompt carries %d runes; the exchange was not truncated", n)
	}
}
