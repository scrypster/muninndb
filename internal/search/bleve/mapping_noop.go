//go:build !vectors

package bleve

import "github.com/blevesearch/bleve/v2/mapping"

func addVectorMapping(*mapping.DocumentMapping, Config) {}
