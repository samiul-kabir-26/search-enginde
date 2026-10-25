package controlplane

import (
	"fmt"
	"net/http"

	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// ExplainerStage describes one pipeline stage for the onboarding diagram (docs/ONBOARDING.md §3).
type ExplainerStage struct {
	ID           string   `json:"id"`
	Number       int      `json:"number"`
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	Configurable []string `json:"configurable"`
	Default      string   `json:"default"`
	Injection    string   `json:"injection"`
	Step         string   `json:"step"`
	// Badge and Choice are filled for a tenant: "Default" or "Custom", and the current choice.
	Badge  string `json:"badge,omitempty"`
	Choice string `json:"choice,omitempty"`
}

// Explainer is the engine explainer document.
type Explainer struct {
	Stages []ExplainerStage `json:"stages"`
}

// BaseExplainer is the static description of the six stages.
func BaseExplainer() Explainer {
	return Explainer{Stages: []ExplainerStage{
		{ID: "query_understanding", Number: 1, Title: "Understand query", Summary: "Normalizes the text, recognizes attributes and price phrases, guesses the category, and turns the query into embeddings.",
			Configurable: []string{"embedding models", "attribute dictionary"}, Default: "bge-small-en-v1.5 for text, clip-vit-b32 for images", Injection: "Embedding models", Step: StepRetrieval},
		{ID: "retrieval", Number: 2, Title: "Find candidates", Summary: "Runs keyword, vector and image searches in parallel with your filters applied, and fuses the lists.",
			Configurable: []string{"vector database", "retriever mix", "field boosts"}, Default: "Platform-hosted index and vector store, balanced mix", Injection: "Your vector DB / index", Step: StepRetrieval},
		{ID: "filter", Number: 3, Title: "Apply your rules", Summary: "Always-on filters (stock, region, status) and custom rules that hide or boost products.",
			Configurable: []string{"templated filters", "custom rules"}, Default: "Active, in-stock products only", Injection: "Policy rules", Step: StepPolicies},
		{ID: "prerank", Number: 4, Title: "Quick scoring", Summary: "A fast model trims thousands of candidates to a few hundred.",
			Configurable: []string{"lightweight ONNX model"}, Default: "Weighted default scorer", Injection: "Lightweight ONNX model", Step: StepRanking},
		{ID: "rank", Number: 5, Title: "Precise ranking", Summary: "Scores the remaining candidates on relevance, quality, seller and popularity signals.",
			Configurable: []string{"feature weights", "ranking model"}, Default: "Weighted default scorer", Injection: "Ranking model", Step: StepRanking},
		{ID: "rerank", Number: 6, Title: "Personalize and place ads", Summary: "Adjusts for the shopper's taste, keeps vendor variety, and places sponsored results.",
			Configurable: []string{"personalization strength", "vendor diversity", "ad slots"}, Default: "Light personalization, 2 results per vendor per 10, ads off", Injection: "Personalization + ads", Step: StepPersonalization},
	}}
}

func (s *Server) handleExplainer(w http.ResponseWriter, _ *http.Request) {
	platform.WriteJSON(w, http.StatusOK, BaseExplainer())
}

func (s *Server) handleTenantExplainer(w http.ResponseWriter, r *http.Request) {
	d, err := s.Q.GetConfigDraft(r.Context(), r.PathValue("tid"))
	if err != nil {
		problem(w, http.StatusNotFound, "no draft")
		return
	}
	cfg, err := tenant.ParseConfig(d.Document)
	if err != nil {
		platform.WriteJSON(w, http.StatusOK, BaseExplainer())
		return
	}
	platform.WriteJSON(w, http.StatusOK, TenantExplainer(cfg))
}

// TenantExplainer overlays a tenant's draft choices on the base explainer.
func TenantExplainer(cfg *tenant.Config) Explainer {
	e := BaseExplainer()
	custom := func(b bool) string {
		if b {
			return "Custom"
		}
		return "Default"
	}
	for i := range e.Stages {
		st := &e.Stages[i]
		switch st.ID {
		case "query_understanding":
			byo := cfg.Embedders.Text.Provider == tenant.ProviderBYO || cfg.Embedders.Image.Provider == tenant.ProviderBYO
			st.Badge, st.Choice = custom(byo || len(cfg.Query.Attributes) > 0), fmt.Sprintf("text: %s, image: %s", embedderName(cfg.Embedders.Text), embedderName(cfg.Embedders.Image))
		case "retrieval":
			byo := cfg.Stores.Vector.Provider == tenant.ProviderBYO || cfg.Stores.Lexical.Provider == tenant.ProviderBYO
			st.Badge, st.Choice = custom(byo), fmt.Sprintf("%d text retrievers, %d image retrievers", len(cfg.Retrieval.Text.Retrievers), len(cfg.Retrieval.Image.Retrievers))
		case "filter":
			st.Badge, st.Choice = custom(len(cfg.Policy.Rules) > 0), fmt.Sprintf("%d always-on filters, %d custom rules", len(cfg.Policy.AlwaysFilters), len(cfg.Policy.Rules))
		case "prerank":
			st.Badge, st.Choice = custom(cfg.Prerank.Scorer.Type != tenant.ScorerDefaultLinear), cfg.Prerank.Scorer.Type
		case "rank":
			st.Badge, st.Choice = custom(cfg.Rank.Scorer.Type != tenant.ScorerDefaultLinear), cfg.Rank.Scorer.Type
		case "rerank":
			byo := cfg.Rerank.Model != nil
			ads := "ads off"
			if cfg.Ads.Enabled {
				ads = fmt.Sprintf("ads in slots %v", cfg.Ads.Slots)
			}
			st.Badge, st.Choice = custom(byo), ads
		}
	}
	return e
}

func embedderName(e tenant.EmbedderConfig) string {
	if e.Provider == tenant.ProviderBYO {
		return "your model " + e.ModelID
	}
	return e.Model
}
