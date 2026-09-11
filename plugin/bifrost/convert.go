package bifrost

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// Причины, по которым запрос не кэшируется. Кэш, который молча отказался
// работать, неотличим от сломанного, поэтому причина считается и логируется.
const (
	bypassMultipleChoices = "multiple_choices"
	bypassTools           = "tools"
	bypassNonTextContent  = "non_text_content"
	bypassNoMessages      = "no_messages"
	bypassStream          = "stream"
)

// cacheKey собирает ключ из всего диалога: ответ зависит от системного промпта
// и предыдущих реплик не меньше, чем от последнего вопроса. Второе значение —
// причина, по которой кэшировать нельзя.
//
// Правила совпадают с правилами semcached намеренно: два адаптера над одним
// кэшем, расходящиеся в том, что кэшируемо, — это два разных кэша.
func cacheKey(req *schemas.BifrostChatRequest, streaming bool) (string, string) {
	if streaming {
		// Попадание можно было бы отдать через LLMPluginShortCircuit.Stream,
		// но пока это не проверено на живом клиенте, честнее не мешать.
		return "", bypassStream
	}
	if req.Params != nil {
		if req.Params.N != nil && *req.Params.N > 1 {
			// Клиент просит несколько разных ответов — кэш вернул бы один.
			return "", bypassMultipleChoices
		}
		if len(req.Params.Tools) > 0 {
			// Результат зависит от описания инструментов и от их выполнения;
			// ключом из одного текста это не описывается.
			return "", bypassTools
		}
	}
	if len(req.Input) == 0 {
		return "", bypassNoMessages
	}

	var b strings.Builder
	for _, m := range req.Input {
		if m.Content == nil || m.Content.ContentStr == nil {
			// Картинку или аудио текстовый ключ не описывает.
			return "", bypassNonTextContent
		}
		b.WriteString(string(m.Role))
		b.WriteString(": ")
		b.WriteString(*m.Content.ContentStr)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), ""
}

// namespaceOf изолирует записи, которые нельзя путать. Прокси разделяет по
// модели; внутри гейтвея в личность записи входит и провайдер: openai и azure с
// одинаковым именем модели — разные развёртывания, и ответ одного на запрос к
// другому был бы подменой, а не попаданием.
func namespaceOf(prefix string, provider schemas.ModelProvider, model string) string {
	return prefix + string(provider) + "/" + model
}

// encodePayload сериализует ответ целиком. В отличие от прокси байты провайдера
// здесь недоступны: хук видит уже разобранную структуру, поэтому в кэш ложится
// она, а попадание её восстанавливает.
func encodePayload(resp *schemas.BifrostChatResponse) (string, error) {
	data, err := json.Marshal(resp)
	if err != nil {
		return "", fmt.Errorf("encode chat response: %w", err)
	}
	return string(data), nil
}

func decodePayload(payload string) (*schemas.BifrostChatResponse, error) {
	var resp schemas.BifrostChatResponse
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return nil, fmt.Errorf("decode chat response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("decode chat response: no choices")
	}
	return &resp, nil
}

// answerText достаёт текст ответа: по нему определяется язык записи, и только
// по нему — в JSON-обёртке языка не видно.
func answerText(resp *schemas.BifrostChatResponse) string {
	for _, c := range resp.Choices {
		if c.ChatNonStreamResponseChoice == nil || c.Message == nil {
			continue
		}
		if content := c.Message.Content; content != nil && content.ContentStr != nil {
			return *content.ContentStr
		}
	}
	return ""
}
