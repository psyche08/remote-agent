package provider

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

func imageAttachment(a Attachment) bool {
	return strings.HasPrefix(a.MediaType, "image/")
}

func attachmentPromptText(prompt string, attachments []Attachment) string {
	lines := []string{}
	for _, a := range attachments {
		if !imageAttachment(a) {
			lines = append(lines, fmt.Sprintf("- %s: %s", a.Name, a.Path))
		}
	}
	if len(lines) == 0 {
		return prompt
	}
	prefix := "Uploaded files available on this device:\n" + strings.Join(lines, "\n")
	if strings.TrimSpace(prompt) == "" {
		return prefix
	}
	return prompt + "\n\n" + prefix
}

func claudeUserContent(prompt string, attachments []Attachment) ([]map[string]any, error) {
	content := []map[string]any{}
	text := attachmentPromptText(prompt, attachments)
	if strings.TrimSpace(text) != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, a := range attachments {
		if !imageAttachment(a) {
			continue
		}
		data, err := os.ReadFile(a.Path)
		if err != nil {
			return nil, fmt.Errorf("read attachment %s: %w", a.Name, err)
		}
		if len(data) > 25*1024*1024 {
			return nil, fmt.Errorf("attachment %s is too large", a.Name)
		}
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "base64", "media_type": a.MediaType,
				"data": base64.StdEncoding.EncodeToString(data),
			},
		})
	}
	if len(content) == 0 {
		return nil, errors.New("prompt and attachments are empty")
	}
	return content, nil
}

func codexUserInput(prompt string, attachments []Attachment) []map[string]any {
	input := []map[string]any{}
	text := attachmentPromptText(prompt, attachments)
	if strings.TrimSpace(text) != "" {
		input = append(input, map[string]any{"type": "text", "text": text, "text_elements": []any{}})
	}
	for _, a := range attachments {
		if imageAttachment(a) {
			input = append(input, map[string]any{"type": "localImage", "path": a.Path})
		}
	}
	return input
}
