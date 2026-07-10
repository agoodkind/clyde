package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
)

type modelCatalogFingerprintRow struct {
	Alias               string
	Backend             string
	WireModel           string
	Context             int
	MaxOutputTokens     int
	Efforts             string
	Effort              string
	ThinkingModes       string
	Thinking            string
	SupportsTools       bool
	SupportsVision      bool
	PassthroughOverride string
	Profile             string
}

func modelCatalogFingerprint(models []adaptermodel.ResolvedAlias) string {
	rows := make([]modelCatalogFingerprintRow, 0, len(models))
	for _, m := range models {
		rows = append(rows, modelCatalogFingerprintRow{
			Alias:               m.Alias,
			Backend:             m.Backend.String(),
			WireModel:           m.WireModel,
			Context:             m.Context,
			MaxOutputTokens:     m.MaxOutputTokens,
			Efforts:             sortedJoined(m.Efforts),
			Effort:              m.Effort,
			ThinkingModes:       sortedJoined(m.ThinkingModes),
			Thinking:            m.Thinking,
			SupportsTools:       m.SupportsTools,
			SupportsVision:      m.SupportsVision,
			PassthroughOverride: m.PassthroughOverride,
			Profile:             m.Profile,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Alias < rows[j].Alias
	})

	var b strings.Builder
	for _, row := range rows {
		_, _ = fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%t\t%t\t%s\t%s\n",
			row.Alias,
			row.Backend,
			row.WireModel,
			row.Context,
			row.MaxOutputTokens,
			row.Efforts,
			row.Effort,
			row.ThinkingModes,
			row.Thinking,
			row.SupportsTools,
			row.SupportsVision,
			row.PassthroughOverride,
			row.Profile,
		)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sortedJoined(values []string) string {
	if len(values) == 0 {
		return ""
	}
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return strings.Join(copied, ",")
}
