package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	"goodkind.io/clyde/internal/config"
)

type modelCatalogFingerprintRow struct {
	Alias               string
	Backend             string
	WireModel           string
	Context             int
	MaxOutputTokens     int
	Efforts             string
	EffortWireValues    string
	Effort              string
	ThinkingModes       string
	Thinking            string
	SupportsTools       bool
	SupportsVision      bool
	PassthroughOverride string
	Profile             string
	TransportLimits     string
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
			EffortWireValues:    sortedStringMap(m.EffortWireValues),
			Effort:              m.Effort,
			ThinkingModes:       sortedJoined(m.ThinkingModes),
			Thinking:            m.Thinking,
			SupportsTools:       m.SupportsTools,
			SupportsVision:      m.SupportsVision,
			PassthroughOverride: m.PassthroughOverride,
			Profile:             m.Profile,
			TransportLimits:     sortedTransportLimits(m.TransportLimits),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Alias < rows[j].Alias
	})

	var b strings.Builder
	for _, row := range rows {
		_, _ = fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%t\t%t\t%s\t%s\t%s\n",
			row.Alias,
			row.Backend,
			row.WireModel,
			row.Context,
			row.MaxOutputTokens,
			row.Efforts,
			row.EffortWireValues,
			row.Effort,
			row.ThinkingModes,
			row.Thinking,
			row.SupportsTools,
			row.SupportsVision,
			row.PassthroughOverride,
			row.Profile,
			row.TransportLimits,
		)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sortedStringMap(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var encoded strings.Builder
	for _, key := range keys {
		value := values[key]
		_, _ = fmt.Fprintf(&encoded, "%d:%s%d:%s", len(key), key, len(value), value)
	}
	return encoded.String()
}

func sortedTransportLimits(limits map[config.AdapterModelTransport]int) string {
	entries := make([]string, 0, len(limits))
	for transport, tokens := range limits {
		entries = append(entries, fmt.Sprintf("%s=%d", transport, tokens))
	}
	sort.Strings(entries)
	return strings.Join(entries, ",")
}

func sortedJoined(values []string) string {
	if len(values) == 0 {
		return ""
	}
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return strings.Join(copied, ",")
}
