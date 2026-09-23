package ai

import "strings"

type Chunk struct {
	Index int
	Text  string
}

func ChunkText(text string, size, overlap int) []Chunk {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if size < 1 {
		size = 1000
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 5
	}
	runes := []rune(text)
	out := make([]Chunk, 0, (len(runes)+size-1)/size)
	start := 0
	for start < len(runes) {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		part := strings.TrimSpace(string(runes[start:end]))
		if part != "" {
			out = append(out, Chunk{Index: len(out), Text: part})
		}
		if end == len(runes) {
			break
		}
		start = end - overlap
	}
	return out
}
