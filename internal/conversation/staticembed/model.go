// Package staticembed embeds text with the potion-base-8M static model. The
// model averages one vector per token and needs no inference runtime.
package staticembed

import (
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/bits"
	"sync"
)

const (
	// Dimensions is the width of every model vector and the bit count of a Code.
	Dimensions = 256
	codeWords  = Dimensions / 64
	// ModelName records which model produced stored codes.
	ModelName = "potion-base-8M"
	// PassageBytes is the largest embedding input in bytes. The model has no
	// token limit; the cap keeps one passage on one topic.
	PassageBytes = 2000
)

//go:embed model/model.safetensors
var safetensorsFile []byte

//go:embed model/tokenizer.json
var tokenizerJSONFile []byte

// Code keeps one sign bit per dimension of a vector minus the token table mean.
// The table mean depends only on the model, so codes from different builds
// compare directly.
type Code [codeWords]uint64

// Distance counts the bits that differ between two codes.
func (code Code) Distance(other Code) int {
	distance := 0
	for word := range codeWords {
		distance += bits.OnesCount64(code[word] ^ other[word])
	}
	return distance
}

// Model is safe for concurrent use.
type Model struct {
	tokenizer *wordPiece
	table     []float32
	mean      [Dimensions]float32
}

var (
	loadOnce   sync.Once
	loaded     *Model
	errLoading error
)

// Load parses the embedded model on the first call.
func Load() (*Model, error) {
	loadOnce.Do(func() {
		loaded, errLoading = parseModel(safetensorsFile, tokenizerJSONFile)
	})
	return loaded, errLoading
}

type safetensorsTensor struct {
	DataType    string `json:"dtype"`
	Shape       []int  `json:"shape"`
	DataOffsets [2]int `json:"data_offsets"`
}

// parseFailed logs one model parse failure and returns the wrapped error.
func parseFailed(operation string, err error) error {
	slog.Error("conversation.staticembed.parse_failed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"operation", operation,
		"err", err,
	)
	return fmt.Errorf("%s: %w", operation, err)
}

func parseModel(weights []byte, tokenizerJSON []byte) (*Model, error) {
	tokenizer, err := parseWordPiece(tokenizerJSON)
	if err != nil {
		return nil, err
	}
	if len(weights) < 8 {
		return nil, errors.New("safetensors file is shorter than its header length")
	}
	encodedLength := binary.LittleEndian.Uint64(weights[:8])
	if encodedLength > math.MaxInt32 || int(encodedLength) > len(weights)-8 {
		return nil, errors.New("safetensors header length exceeds the file")
	}
	headerLength := int(encodedLength)
	var header map[string]json.RawMessage
	if err := json.Unmarshal(weights[8:8+headerLength], &header); err != nil {
		return nil, parseFailed("parse safetensors header", err)
	}
	var tensor safetensorsTensor
	if err := json.Unmarshal(header["embeddings"], &tensor); err != nil {
		return nil, parseFailed("parse the embeddings tensor header", err)
	}
	if tensor.DataType != "F32" || len(tensor.Shape) != 2 || tensor.Shape[1] != Dimensions {
		shapeErr := fmt.Errorf("embeddings tensor is %s %v, want F32 [rows %d]", tensor.DataType, tensor.Shape, Dimensions)
		slog.Error("conversation.staticembed.parse_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"operation", "check the embeddings tensor shape",
			"err", shapeErr,
		)
		return nil, shapeErr
	}
	data := weights[8+headerLength:]
	start, end := tensor.DataOffsets[0], tensor.DataOffsets[1]
	if start < 0 || end > len(data) || end-start != tensor.Shape[0]*Dimensions*4 {
		return nil, errors.New("embeddings tensor offsets do not match its shape")
	}
	table := make([]float32, tensor.Shape[0]*Dimensions)
	for position := range table {
		table[position] = math.Float32frombits(binary.LittleEndian.Uint32(data[start+position*4:]))
	}
	model := &Model{tokenizer: tokenizer, table: table, mean: [Dimensions]float32{}}
	for row := range tensor.Shape[0] {
		for dimension := range Dimensions {
			model.mean[dimension] += table[row*Dimensions+dimension]
		}
	}
	for dimension := range Dimensions {
		model.mean[dimension] /= float32(tensor.Shape[0])
	}
	return model, nil
}

// Vector returns the mean token vector of text minus the token table mean.
// Text with no known token returns the zero vector.
func (model *Model) Vector(text string) []float32 {
	vector := make([]float32, Dimensions)
	ids := model.tokenizer.appendIDs(make([]int32, 0, len(text)/4+1), text)
	if len(ids) == 0 {
		return vector
	}
	for _, id := range ids {
		row := model.table[int(id)*Dimensions : int(id)*Dimensions+Dimensions]
		for dimension := range Dimensions {
			vector[dimension] += row[dimension]
		}
	}
	count := float32(len(ids))
	for dimension := range Dimensions {
		vector[dimension] = vector[dimension]/count - model.mean[dimension]
	}
	return vector
}

// Code returns the sign code of Vector(text).
func (model *Model) Code(text string) Code {
	return CodeOf(model.Vector(text))
}

// CodeOf sets one bit for each positive component of vector. Components past
// Dimensions are ignored.
func CodeOf(vector []float32) Code {
	var code Code
	for dimension := range min(len(vector), Dimensions) {
		if vector[dimension] > 0 {
			code[dimension/64] |= 1 << (dimension % 64)
		}
	}
	return code
}
