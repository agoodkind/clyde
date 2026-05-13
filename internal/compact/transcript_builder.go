package compact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"goodkind.io/clyde/internal/contextcount"
)

type transcriptBuilderFunc func(
	*Slice,
	RuntimeUpfront,
	[]byte,
	[]byte,
) (contextcount.Transcript, error)

var transcriptBuilderAnchor transcriptBuilderFunc

func init() {
	// Commit 5 stages the builder before commit 6 wires it into planning.
	// Keep the symbol reachable so the repo deadcode gate can still run.
	transcriptBuilderAnchor = BuildTranscript
	if transcriptBuilderAnchor == nil {
		panic("compact transcript builder is nil")
	}
}

// BuildTranscript converts a compact slice into the typed transcript consumed by context counters.
func BuildTranscript(
	slice *Slice,
	upfront RuntimeUpfront,
	capturedSystem []byte,
	capturedTools []byte,
) (contextcount.Transcript, error) {
	if slice == nil {
		return contextcount.Transcript{}, errors.New("compact: build transcript: nil slice")
	}

	transcript := contextcount.Transcript{
		Model:    upfront.Model,
		System:   nil,
		Tools:    nil,
		Messages: nil,
	}
	systemBlocks, err := unmarshalCapturedSystem(capturedSystem)
	if err != nil {
		return contextcount.Transcript{}, err
	}
	transcript.System = append(transcript.System, systemBlocks...)

	tools, err := unmarshalCapturedTools(capturedTools)
	if err != nil {
		return contextcount.Transcript{}, err
	}
	transcript.Tools = append(transcript.Tools, tools...)

	messages, err := unmarshalTranscriptMessages(slice.PostBoundary)
	if err != nil {
		return contextcount.Transcript{}, err
	}
	transcript.Messages = messages

	if err := readMemoryAndSkillBlocks(&transcript, slice); err != nil {
		return contextcount.Transcript{}, err
	}
	return transcript, nil
}

type capturedMessagesRequest struct {
	System json.RawMessage   `json:"system"`
	Tools  []capturedToolDef `json:"tools"`
}

type capturedSystemBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type capturedToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type transcriptBlockType string

const (
	transcriptBlockText             transcriptBlockType = "text"
	transcriptBlockThinking         transcriptBlockType = "thinking"
	transcriptBlockRedactedThinking transcriptBlockType = "redacted_thinking"
	transcriptBlockToolUse          transcriptBlockType = "tool_use"
	transcriptBlockToolResult       transcriptBlockType = "tool_result"
	transcriptBlockImage            transcriptBlockType = "image"
)

func unmarshalCapturedSystem(data []byte) ([]contextcount.TextBlock, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] != '{' {
		blocks, err := unmarshalSystemBlocks(trimmed)
		if err != nil {
			slog.Warn("compact.transcript_builder.captured_system_failed",
				"component", "compact",
				"err", err,
			)
			return nil, fmt.Errorf("compact: parse captured system: %w", err)
		}
		return blocks, nil
	}

	var request capturedMessagesRequest
	if err := json.Unmarshal(trimmed, &request); err != nil {
		slog.Warn("compact.transcript_builder.captured_system_request_failed",
			"component", "compact",
			"err", err,
		)
		return nil, fmt.Errorf("compact: parse captured request system: %w", err)
	}
	if len(bytes.TrimSpace(request.System)) == 0 {
		return nil, nil
	}
	blocks, err := unmarshalSystemBlocks(request.System)
	if err != nil {
		slog.Warn("compact.transcript_builder.captured_system_request_failed",
			"component", "compact",
			"err", err,
		)
		return nil, fmt.Errorf("compact: parse captured request system: %w", err)
	}
	return blocks, nil
}

func unmarshalCapturedTools(data []byte) ([]contextcount.ToolDef, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var tools []capturedToolDef
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &tools); err != nil {
			slog.Warn("compact.transcript_builder.captured_tools_failed",
				"component", "compact",
				"err", err,
			)
			return nil, fmt.Errorf("compact: parse captured tools: %w", err)
		}
		return contextToolDefs(tools), nil
	}

	var request capturedMessagesRequest
	if err := json.Unmarshal(trimmed, &request); err != nil {
		slog.Warn("compact.transcript_builder.captured_tools_request_failed",
			"component", "compact",
			"err", err,
		)
		return nil, fmt.Errorf("compact: parse captured request tools: %w", err)
	}
	return contextToolDefs(request.Tools), nil
}

func unmarshalSystemBlocks(raw json.RawMessage) ([]contextcount.TextBlock, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			slog.Warn("compact.transcript_builder.system_string_failed",
				"component", "compact",
				"err", err,
			)
			return nil, fmt.Errorf("decode system string: %w", err)
		}
		return []contextcount.TextBlock{{Text: text}}, nil
	case '{':
		var block capturedSystemBlock
		if err := json.Unmarshal(trimmed, &block); err != nil {
			slog.Warn("compact.transcript_builder.system_block_failed",
				"component", "compact",
				"err", err,
			)
			return nil, fmt.Errorf("decode system block: %w", err)
		}
		return []contextcount.TextBlock{{Text: block.Text}}, nil
	case '[':
		var blocks []capturedSystemBlock
		if err := json.Unmarshal(trimmed, &blocks); err != nil {
			slog.Warn("compact.transcript_builder.system_blocks_failed",
				"component", "compact",
				"err", err,
			)
			return nil, fmt.Errorf("decode system blocks: %w", err)
		}
		systemBlocks := make([]contextcount.TextBlock, 0, len(blocks))
		for _, block := range blocks {
			systemBlocks = append(systemBlocks, contextcount.TextBlock{Text: block.Text})
		}
		return systemBlocks, nil
	default:
		return nil, fmt.Errorf("unsupported system shape %q", string(trimmed[:1]))
	}
}

func contextToolDefs(tools []capturedToolDef) []contextcount.ToolDef {
	out := make([]contextcount.ToolDef, 0, len(tools))
	for _, tool := range tools {
		out = append(out, contextcount.ToolDef{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: cloneRawMessage(tool.InputSchema),
		})
	}
	return out
}

func unmarshalTranscriptMessages(entries []Entry) ([]contextcount.Message, error) {
	messages := make([]contextcount.Message, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}
		if isSidechainEntry(entry) {
			continue
		}
		content, err := unmarshalTranscriptContentEntry(entry)
		if err != nil {
			slog.Warn("compact.transcript_builder.entry_content_failed",
				"component", "compact",
				"line", entry.LineIndex,
				"err", err,
			)
			return nil, fmt.Errorf("compact: entry line %d: %w", entry.LineIndex, err)
		}
		role := entry.Role
		if role == "" {
			role = entry.Type
		}
		messages = append(messages, contextcount.Message{
			Role:    role,
			Content: content,
		})
	}
	return messages, nil
}

func isSidechainEntry(entry Entry) bool {
	if len(bytes.TrimSpace(entry.Raw)) == 0 {
		return false
	}
	var top struct {
		IsSidechain bool `json:"isSidechain"`
	}
	if err := json.Unmarshal(entry.Raw, &top); err != nil {
		return false
	}
	return top.IsSidechain
}

func unmarshalTranscriptContentEntry(entry Entry) ([]contextcount.ContentBlock, error) {
	rawContent, ok, err := unmarshalMessageContentRaw(entry.Raw)
	if err != nil {
		return nil, err
	}
	if ok {
		return unmarshalTranscriptContent(rawContent)
	}
	return unmarshalTranscriptContentParsedEntry(entry)
}

func unmarshalMessageContentRaw(raw []byte) (json.RawMessage, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, nil
	}
	var top struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		slog.Warn("compact.transcript_builder.message_content_failed",
			"component", "compact",
			"err", err,
		)
		return nil, false, fmt.Errorf("decode message content: %w", err)
	}
	if len(top.Message.Content) == 0 {
		return nil, false, nil
	}
	return top.Message.Content, true, nil
}

func unmarshalTranscriptContent(raw json.RawMessage) ([]contextcount.ContentBlock, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			slog.Warn("compact.transcript_builder.text_content_failed",
				"component", "compact",
				"err", err,
			)
			return nil, fmt.Errorf("decode text content: %w", err)
		}
		return []contextcount.ContentBlock{newTextContentBlock(text)}, nil
	case '[':
		var blocks []json.RawMessage
		if err := json.Unmarshal(trimmed, &blocks); err != nil {
			slog.Warn("compact.transcript_builder.content_blocks_failed",
				"component", "compact",
				"err", err,
			)
			return nil, fmt.Errorf("decode content blocks: %w", err)
		}
		out := make([]contextcount.ContentBlock, 0, len(blocks))
		for _, block := range blocks {
			contentBlock, err := unmarshalTranscriptBlock(block)
			if err != nil {
				return nil, err
			}
			out = append(out, contentBlock)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported message content shape %q", string(trimmed[:1]))
	}
}

func unmarshalTranscriptBlock(raw json.RawMessage) (contextcount.ContentBlock, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		slog.Warn("compact.transcript_builder.content_block_type_failed",
			"component", "compact",
			"err", err,
		)
		return contextcount.ContentBlock{}, fmt.Errorf("decode content block type: %w", err)
	}
	switch transcriptBlockType(head.Type) {
	case transcriptBlockText:
		return unmarshalTextBlock(raw)
	case transcriptBlockThinking:
		return unmarshalThinkingBlock(raw)
	case transcriptBlockRedactedThinking:
		return unmarshalRedactedThinkingBlock(raw)
	case transcriptBlockToolUse:
		return unmarshalToolUseBlock(raw)
	case transcriptBlockToolResult:
		return unmarshalToolResultBlock(raw)
	case transcriptBlockImage:
		return unmarshalImageBlock(raw)
	default:
		return contextcount.ContentBlock{}, fmt.Errorf("unsupported content block type %q", head.Type)
	}
}

func unmarshalTextBlock(raw json.RawMessage) (contextcount.ContentBlock, error) {
	var block struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		slog.Warn("compact.transcript_builder.text_block_failed",
			"component", "compact",
			"err", err,
		)
		return contextcount.ContentBlock{}, fmt.Errorf("decode text block: %w", err)
	}
	return newTextContentBlock(block.Text), nil
}

func unmarshalThinkingBlock(raw json.RawMessage) (contextcount.ContentBlock, error) {
	var block struct {
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		slog.Warn("compact.transcript_builder.thinking_block_failed",
			"component", "compact",
			"err", err,
		)
		return contextcount.ContentBlock{}, fmt.Errorf("decode thinking block: %w", err)
	}
	return newThinkingContentBlock(block.Thinking, block.Signature), nil
}

func unmarshalRedactedThinkingBlock(raw json.RawMessage) (contextcount.ContentBlock, error) {
	var block struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		slog.Warn("compact.transcript_builder.redacted_thinking_block_failed",
			"component", "compact",
			"err", err,
		)
		return contextcount.ContentBlock{}, fmt.Errorf("decode redacted thinking block: %w", err)
	}
	return newRedactedThinkingContentBlock(block.Data), nil
}

func unmarshalToolUseBlock(raw json.RawMessage) (contextcount.ContentBlock, error) {
	var block struct {
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		slog.Warn("compact.transcript_builder.tool_use_block_failed",
			"component", "compact",
			"err", err,
		)
		return contextcount.ContentBlock{}, fmt.Errorf("decode tool_use block: %w", err)
	}
	return newToolUseContentBlock(block.ID, block.Name, block.Input), nil
}

func unmarshalToolResultBlock(raw json.RawMessage) (contextcount.ContentBlock, error) {
	var block struct {
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		slog.Warn("compact.transcript_builder.tool_result_block_failed",
			"component", "compact",
			"err", err,
		)
		return contextcount.ContentBlock{}, fmt.Errorf("decode tool_result block: %w", err)
	}
	contentBlock := newToolResultContentBlock(block.ToolUseID, block.IsError)
	trimmed := bytes.TrimSpace(block.Content)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return contentBlock, nil
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			slog.Warn("compact.transcript_builder.tool_result_text_failed",
				"component", "compact",
				"err", err,
			)
			return contextcount.ContentBlock{}, fmt.Errorf("decode tool_result text: %w", err)
		}
		contentBlock.ToolResultText = text
	case '[':
		nestedBlocks, err := unmarshalTranscriptContent(trimmed)
		if err != nil {
			slog.Warn("compact.transcript_builder.tool_result_content_failed",
				"component", "compact",
				"err", err,
			)
			return contextcount.ContentBlock{}, fmt.Errorf("decode tool_result content: %w", err)
		}
		contentBlock.ToolResult = nestedBlocks
	default:
		return contextcount.ContentBlock{}, fmt.Errorf(
			"unsupported tool_result content shape %q",
			string(trimmed[:1]),
		)
	}
	return contentBlock, nil
}

func unmarshalImageBlock(raw json.RawMessage) (contextcount.ContentBlock, error) {
	var block struct {
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		slog.Warn("compact.transcript_builder.image_block_failed",
			"component", "compact",
			"err", err,
		)
		return contextcount.ContentBlock{}, fmt.Errorf("decode image block: %w", err)
	}
	return newImageContentBlock(block.Source.Type, block.Source.MediaType, block.Source.Data, block.Source.URL), nil
}

func unmarshalTranscriptContentParsedEntry(entry Entry) ([]contextcount.ContentBlock, error) {
	contentBlocks := make([]contextcount.ContentBlock, 0, len(entry.Content)+1)
	if entry.TextOnly != "" {
		contentBlocks = append(contentBlocks, newTextContentBlock(entry.TextOnly))
	}
	for _, block := range entry.Content {
		contentBlock, err := unmarshalTranscriptBlockParsed(block)
		if err != nil {
			return nil, err
		}
		contentBlocks = append(contentBlocks, contentBlock)
	}
	return contentBlocks, nil
}

func unmarshalTranscriptBlockParsed(block ContentBlock) (contextcount.ContentBlock, error) {
	if len(bytes.TrimSpace(block.Raw)) > 0 {
		return unmarshalTranscriptBlock(block.Raw)
	}
	switch transcriptBlockType(block.Type) {
	case transcriptBlockText:
		return newTextContentBlock(block.Text), nil
	case transcriptBlockThinking:
		return newThinkingContentBlock(block.Thinking, ""), nil
	case transcriptBlockRedactedThinking:
		return newRedactedThinkingContentBlock(block.Thinking), nil
	case transcriptBlockToolUse:
		return newToolUseContentBlock(block.ToolUseID, block.ToolName, block.ToolInput), nil
	case transcriptBlockToolResult:
		return unmarshalParsedToolResultBlock(block)
	case transcriptBlockImage:
		return newImageContentBlock("base64", block.ImageMediaType, block.ImageDataB64, ""), nil
	default:
		return contextcount.ContentBlock{}, fmt.Errorf("unsupported content block type %q", block.Type)
	}
}

func unmarshalParsedToolResultBlock(block ContentBlock) (contextcount.ContentBlock, error) {
	contentBlock := newToolResultContentBlock(block.ToolUseRefID, block.ToolIsError)
	if len(block.ToolContent) == 1 && block.ToolContent[0].Type == "text" {
		contentBlock.ToolResultText = block.ToolContent[0].Text
		return contentBlock, nil
	}
	nestedBlocks := make([]contextcount.ContentBlock, 0, len(block.ToolContent))
	for _, nested := range block.ToolContent {
		nestedBlock, err := unmarshalTranscriptBlockParsed(nested)
		if err != nil {
			return contextcount.ContentBlock{}, err
		}
		nestedBlocks = append(nestedBlocks, nestedBlock)
	}
	contentBlock.ToolResult = nestedBlocks
	return contentBlock, nil
}

func readMemoryAndSkillBlocks(transcript *contextcount.Transcript, slice *Slice) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("compact.transcript_builder.home_dir_failed",
			"component", "compact",
			"err", err,
		)
		return fmt.Errorf("compact: resolve home dir: %w", err)
	}
	workspaceRoot := workspaceRootFromSlice(slice)
	memoryFiles := memoryFilePaths(homeDir, workspaceRoot, slice.Path)
	for _, path := range memoryFiles {
		if err := readTextFileIfExists(transcript, path); err != nil {
			return err
		}
	}
	skillRoots := skillRootPaths(homeDir, workspaceRoot)
	for _, root := range skillRoots {
		if err := readSkillFiles(transcript, root); err != nil {
			return err
		}
	}
	return nil
}

func memoryFilePaths(homeDir, workspaceRoot, transcriptPath string) []string {
	paths := []string{filepath.Join(homeDir, ".claude", "CLAUDE.md")}
	if workspaceRoot != "" {
		paths = append(paths, filepath.Join(workspaceRoot, "CLAUDE.md"))
	}
	projectDir := encodedProjectDir(workspaceRoot, transcriptPath)
	if projectDir != "" {
		paths = append(
			paths,
			filepath.Join(homeDir, ".claude", "projects", projectDir, "memory", "MEMORY.md"),
		)
	}
	return paths
}

func skillRootPaths(homeDir, workspaceRoot string) []string {
	paths := []string{filepath.Join(homeDir, ".claude", "skills")}
	if workspaceRoot != "" {
		paths = append(paths, filepath.Join(workspaceRoot, ".claude", "skills"))
	}
	return paths
}

func readTextFileIfExists(transcript *contextcount.Transcript, path string) error {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		slog.Warn("compact.transcript_builder.system_file_read_failed",
			"component", "compact",
			"path", path,
			"err", err,
		)
		return fmt.Errorf("compact: read system file %s: %w", path, err)
	}
	transcript.System = append(transcript.System, contextcount.TextBlock{Text: string(content)})
	return nil
}

func readSkillFiles(transcript *contextcount.Transcript, root string) error {
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		slog.Warn("compact.transcript_builder.skill_root_stat_failed",
			"component", "compact",
			"root", root,
			"err", err,
		)
		return fmt.Errorf("compact: stat skill root %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil
	}

	paths := []string{}
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if walkErr != nil {
		slog.Warn("compact.transcript_builder.skill_root_walk_failed",
			"component", "compact",
			"root", root,
			"err", walkErr,
		)
		return fmt.Errorf("compact: walk skill root %s: %w", root, walkErr)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := readTextFileIfExists(transcript, path); err != nil {
			return err
		}
	}
	return nil
}

func workspaceRootFromSlice(slice *Slice) string {
	if slice == nil {
		return ""
	}
	for _, entry := range slice.AllEntries {
		cwd := entryCWD(entry)
		if cwd != "" {
			return cwd
		}
	}
	return ""
}

func entryCWD(entry Entry) string {
	if len(bytes.TrimSpace(entry.Raw)) == 0 {
		return ""
	}
	var top struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(entry.Raw, &top); err != nil {
		return ""
	}
	return strings.TrimSpace(top.CWD)
}

func encodedProjectDir(workspaceRoot, transcriptPath string) string {
	if workspaceRoot != "" {
		return encodeClaudeProjectPath(workspaceRoot)
	}
	parentDir := filepath.Dir(transcriptPath)
	if parentDir == "" || parentDir == "." {
		return ""
	}
	projectDir := filepath.Base(parentDir)
	if projectDir == "." || projectDir == string(filepath.Separator) {
		return ""
	}
	return projectDir
}

func encodeClaudeProjectPath(projectPath string) string {
	encoded := strings.ReplaceAll(projectPath, "/", "-")
	return strings.ReplaceAll(encoded, ".", "-")
}

func newTextContentBlock(text string) contextcount.ContentBlock {
	return contextcount.ContentBlock{
		Type:           string(transcriptBlockText),
		Text:           text,
		Thinking:       "",
		Signature:      "",
		RedactedData:   "",
		ToolUseID:      "",
		ToolName:       "",
		ToolInput:      nil,
		ToolResult:     nil,
		ToolResultText: "",
		IsError:        false,
		ImageSource:    emptyImageSource(),
	}
}

func newThinkingContentBlock(thinking, signature string) contextcount.ContentBlock {
	block := newTextContentBlock("")
	block.Type = string(transcriptBlockThinking)
	block.Thinking = thinking
	block.Signature = signature
	return block
}

func newRedactedThinkingContentBlock(data string) contextcount.ContentBlock {
	block := newTextContentBlock("")
	block.Type = string(transcriptBlockRedactedThinking)
	block.RedactedData = data
	return block
}

func newToolUseContentBlock(id, name string, input json.RawMessage) contextcount.ContentBlock {
	block := newTextContentBlock("")
	block.Type = string(transcriptBlockToolUse)
	block.ToolUseID = id
	block.ToolName = name
	block.ToolInput = cloneRawMessage(input)
	return block
}

func newToolResultContentBlock(toolUseID string, isError bool) contextcount.ContentBlock {
	block := newTextContentBlock("")
	block.Type = string(transcriptBlockToolResult)
	block.ToolUseID = toolUseID
	block.IsError = isError
	return block
}

func newImageContentBlock(sourceType, mediaType, data, url string) contextcount.ContentBlock {
	block := newTextContentBlock("")
	block.Type = string(transcriptBlockImage)
	block.ImageSource = contextcount.ImageSource{
		Type:      sourceType,
		MediaType: mediaType,
		Data:      data,
		URL:       url,
	}
	return block
}

func emptyImageSource() contextcount.ImageSource {
	return contextcount.ImageSource{
		Type:      "",
		MediaType: "",
		Data:      "",
		URL:       "",
	}
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
