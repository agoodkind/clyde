package main

// These blank imports link the shared search library packages into the clyde
// binary before the embedded ingestion code imports them. The release compile
// jobs then build their cgo dependencies for every release platform. The
// ingestion code replaces this file.
import (
	_ "goodkind.io/lm-semantic-search/library"
	_ "goodkind.io/lm-semantic-search/library/embedding"
	_ "goodkind.io/lm-semantic-search/library/milvus"
)
