package parser

import (
	"encoding/json"

	"goodkind.io/clyde/internal/transcript"
)

type attachmentMetadata struct {
	Type          string `json:"type"`
	DisplayName   string `json:"displayName"`
	Path          string `json:"path"`
	FilePath      string `json:"filePath"`
	URL           string `json:"url"`
	MIMEType      string `json:"mimeType"`
	ByteLength    int64  `json:"byteLength"`
	Description   string `json:"description"`
	AssetID       string `json:"assetId"`
	OmittedReason string `json:"omittedReason"`
	Text          string `json:"text"`
	Title         string `json:"title"`
	Name          string `json:"name"`
	Message       string `json:"message"`
	JobName       string `json:"jobName"`
}

func mapAttachments(rawAttachments []json.RawMessage) []transcript.Attachment {
	attachments := make([]transcript.Attachment, 0, len(rawAttachments))
	for _, raw := range rawAttachments {
		var metadata attachmentMetadata
		if json.Unmarshal(raw, &metadata) != nil || metadata.Type == "" {
			continue
		}
		attachments = append(attachments, transcript.Attachment{
			Kind: metadata.Type,
			DisplayName: firstNonEmpty(
				metadata.DisplayName,
				metadata.Title,
				metadata.Name,
				metadata.JobName,
			),
			Path:           firstNonEmpty(metadata.Path, metadata.FilePath),
			URL:            metadata.URL,
			MIMEType:       metadata.MIMEType,
			SizeBytes:      metadata.ByteLength,
			Description:    firstNonEmpty(metadata.Description, metadata.Message),
			AssetReference: metadata.AssetID,
			OmittedReason:  metadata.OmittedReason,
			Text:           metadata.Text,
		})
	}
	return attachments
}
