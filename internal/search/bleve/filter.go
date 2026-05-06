package bleve

import (
	"fmt"
	"time"

	blevesearch "github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/scrypster/muninndb/internal/search"
)

// buildFilterQuery converts activation filters into a bleve filter query for
// use with AddKNNWithFilter. Returns nil when there are no supported filters.
func buildFilterQuery(filters []search.Filter) query.Query {
	var clauses []query.Query

	for _, f := range filters {
		switch f.Field {
		case "created_by":
			if f.Op == "eq" || f.Op == "" {
				q := blevesearch.NewTermQuery(fmt.Sprint(f.Value))
				q.SetField("created_by")
				clauses = append(clauses, q)
			}
		case "tags":
			if f.Op == "has" || f.Op == "eq" || f.Op == "" {
				q := blevesearch.NewTermQuery(fmt.Sprint(f.Value))
				q.SetField("tags")
				clauses = append(clauses, q)
			}
		case "created_after":
			if epoch := toEpoch(f.Value); epoch > 0 {
				nq := blevesearch.NewNumericRangeInclusiveQuery(&epoch, nil, boolPtr(true), nil)
				nq.SetField("created_at")
				clauses = append(clauses, nq)
			}
		case "created_before":
			if epoch := toEpoch(f.Value); epoch > 0 {
				nq := blevesearch.NewNumericRangeInclusiveQuery(nil, &epoch, nil, boolPtr(false))
				nq.SetField("created_at")
				clauses = append(clauses, nq)
			}
		}
	}

	if len(clauses) == 0 {
		return nil
	}
	if len(clauses) == 1 {
		return clauses[0]
	}
	return blevesearch.NewConjunctionQuery(clauses...)
}

func toEpoch(v any) float64 {
	switch t := v.(type) {
	case time.Time:
		return float64(t.Unix())
	case string:
		parsed, err := time.Parse(time.RFC3339, t)
		if err != nil {
			return 0
		}
		return float64(parsed.Unix())
	case float64:
		return t
	case int64:
		return float64(t)
	default:
		return 0
	}
}

func boolPtr(b bool) *bool { return &b }
