package bifrost

import (
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

const (
	objectChunk    = "chat.completion.chunk"
	objectResponse = "chat.completion"
	roleAssistant  = "assistant"
	finishStop     = "stop"
)

// replayChunk превращает закэшированный ответ в один чанк со всем текстом и
// finish_reason. Резать его на много частей значило бы изображать генерацию,
// которой не было: текст уже целиком лежит в памяти, и задержка тут нулевая.
func replayChunk(resp *schemas.BifrostChatResponse) *schemas.BifrostStreamChunk {
	text := answerText(resp)
	role := roleAssistant
	finish := finishStop
	if fr := finishReasonOf(resp); fr != "" {
		finish = fr
	}

	return &schemas.BifrostStreamChunk{BifrostChatResponse: &schemas.BifrostChatResponse{
		ID:                resp.ID,
		Model:             resp.Model,
		Object:            objectChunk,
		Created:           int(time.Now().Unix()),
		SystemFingerprint: resp.SystemFingerprint,
		Usage:             resp.Usage,
		Choices: []schemas.BifrostResponseChoice{{
			Index:        0,
			FinishReason: &finish,
			ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
				Delta: &schemas.ChatStreamResponseChoiceDelta{Role: &role, Content: &text},
			},
		}},
	}}
}

// streamAccumulator собирает ответ из дельт. Синхронизации нет намеренно: core
// дренирует поток одной горутиной и вызывает PostLLMHook по чанку за раз, так
// что на один запрос здесь один писатель.
type streamAccumulator struct {
	text   strings.Builder
	last   *schemas.BifrostChatResponse
	finish string
}

// add забирает из чанка текст дельты и всё, что нужно для сборки ответа:
// usage приходит в последнем чанке, finish_reason — в предпоследнем.
func (a *streamAccumulator) add(chunk *schemas.BifrostChatResponse) {
	if chunk == nil {
		return
	}
	a.last = chunk
	for _, c := range chunk.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			a.finish = *c.FinishReason
		}
		if c.ChatStreamResponseChoice == nil || c.ChatStreamResponseChoice.Delta == nil {
			continue
		}
		if content := c.ChatStreamResponseChoice.Delta.Content; content != nil {
			a.text.WriteString(*content)
		}
	}
}

// assemble собирает из дельт ответ в нестримовой форме.
//
// Это ещё один шаг вниз по точности: у стримового промаха кэшируется не ответ
// провайдера, а наша сборка из его дельт. Иначе стримящий клиент не наполнит
// кэш вообще, и на развёртывании, где стримят все, кэш останется пустым.
func (a *streamAccumulator) assemble() *schemas.BifrostChatResponse {
	text := a.text.String()
	if text == "" || a.last == nil {
		return nil
	}
	role := schemas.ChatMessageRoleAssistant
	finish := a.finish
	if finish == "" {
		finish = finishStop
	}

	return &schemas.BifrostChatResponse{
		ID:                a.last.ID,
		Model:             a.last.Model,
		Object:            objectResponse,
		Created:           a.last.Created,
		SystemFingerprint: a.last.SystemFingerprint,
		Usage:             a.last.Usage,
		Choices: []schemas.BifrostResponseChoice{{
			Index:        0,
			FinishReason: &finish,
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
				Message: &schemas.ChatMessage{
					Role:    role,
					Content: &schemas.ChatMessageContent{ContentStr: &text},
				},
			},
		}},
	}
}

// isStreamChunk отличает чанк от готового ответа по виду choice, а не по
// полю Object: второе провайдеры заполняют не одинаково.
func isStreamChunk(resp *schemas.BifrostChatResponse) bool {
	for _, c := range resp.Choices {
		if c.ChatStreamResponseChoice != nil {
			return true
		}
	}
	return false
}

func finishReasonOf(resp *schemas.BifrostChatResponse) string {
	for _, c := range resp.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			return *c.FinishReason
		}
	}
	return ""
}
