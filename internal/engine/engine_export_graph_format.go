package engine

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
)

// FormatGraphJSONLD serialises g into JSON-LD format and returns it as a string.
func FormatGraphJSONLD(g *ExportGraph) (string, error) {
	graph := make([]map[string]any, 0, len(g.Nodes)+len(g.Edges))

	for _, node := range g.Nodes {
		n := map[string]any{
			"@type": "muninn:Entity",
			"@id":   "muninn:entity/" + node.ID,
			"name":  node.ID,
		}
		if node.Type != "" {
			n["muninn:entityType"] = node.Type
		}
		graph = append(graph, n)
	}

	for i, edge := range g.Edges {
		e := map[string]any{
			"@type":          "muninn:Relationship",
			"@id":            fmt.Sprintf("muninn:rel/%s/%s/%s", edge.From, edge.RelType, edge.To),
			"muninn:from":    "muninn:entity/" + edge.From,
			"muninn:to":      "muninn:entity/" + edge.To,
			"muninn:relType": edge.RelType,
			"muninn:weight":  edge.Weight,
		}
		_ = i
		graph = append(graph, e)
	}

	doc := map[string]any{
		"@context": map[string]any{
			"@vocab": "https://schema.org/",
			"muninn": "https://muninndb.io/ontology#",
		},
		"@graph": graph,
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("format json-ld: %w", err)
	}
	return string(b), nil
}

// graphMLDoc is the top-level XML document for GraphML serialisation.
type graphMLDoc struct {
	XMLName xml.Name     `xml:"graphml"`
	Xmlns   string       `xml:"xmlns,attr"`
	Keys    []graphMLKey `xml:"key"`
	Graph   graphMLGraph `xml:"graph"`
}

type graphMLKey struct {
	ID       string `xml:"id,attr"`
	For      string `xml:"for,attr"`
	AttrName string `xml:"attr.name,attr"`
	AttrType string `xml:"attr.type,attr"`
}

type graphMLGraph struct {
	ID          string        `xml:"id,attr"`
	EdgeDefault string        `xml:"edgedefault,attr"`
	Nodes       []graphMLNode `xml:"node"`
	Edges       []graphMLEdge `xml:"edge"`
}

type graphMLNode struct {
	ID   string        `xml:"id,attr"`
	Data []graphMLData `xml:"data"`
}

type graphMLEdge struct {
	ID     string        `xml:"id,attr"`
	Source string        `xml:"source,attr"`
	Target string        `xml:"target,attr"`
	Data   []graphMLData `xml:"data"`
}

type graphMLData struct {
	Key   string `xml:"key,attr"`
	Value string `xml:",chardata"`
}

// FormatGraphGraphML serialises g into GraphML XML format and returns it as a string.
func FormatGraphGraphML(g *ExportGraph) (string, error) {
	doc := graphMLDoc{
		Xmlns: "http://graphml.graphdrawing.org/graphml",
		Keys: []graphMLKey{
			{ID: "type", For: "node", AttrName: "type", AttrType: "string"},
			{ID: "weight", For: "edge", AttrName: "weight", AttrType: "double"},
			{ID: "reltype", For: "edge", AttrName: "reltype", AttrType: "string"},
		},
		Graph: graphMLGraph{
			ID:          "G",
			EdgeDefault: "directed",
		},
	}

	for _, node := range g.Nodes {
		n := graphMLNode{ID: node.ID}
		if node.Type != "" {
			n.Data = append(n.Data, graphMLData{Key: "type", Value: node.Type})
		}
		doc.Graph.Nodes = append(doc.Graph.Nodes, n)
	}

	for i, edge := range g.Edges {
		e := graphMLEdge{
			ID:     fmt.Sprintf("e%d", i),
			Source: edge.From,
			Target: edge.To,
			Data: []graphMLData{
				{Key: "weight", Value: fmt.Sprintf("%g", edge.Weight)},
				{Key: "reltype", Value: edge.RelType},
			},
		}
		doc.Graph.Edges = append(doc.Graph.Edges, e)
	}

	b, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("format graphml: %w", err)
	}
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteByte('\n')
	sb.Write(b)
	return sb.String(), nil
}
