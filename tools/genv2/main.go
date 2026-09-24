// Command genv2 собирает bench/dataset/v2.jsonl поверх v1.
//
// У каждой пары появляется ответ, записанный для B, и язык этого ответа.
// Гейт по короткому запросу на v1 не мог измерить цену recall: все
// взаимозаменяемые пары были английскими. Здесь на каждом языке детектора
// есть положительные пары, а language_switch несёт ответ на языке B.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"unicode"

	"github.com/mytholog/semcache/internal/dataset"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	in := filepath.Join(root, "bench/dataset/v1.jsonl")
	out := filepath.Join(root, "bench/dataset/v2.jsonl")
	if err := generate(in, out); err != nil {
		fmt.Fprintf(os.Stderr, "genv2: %v\n", err)
		os.Exit(1)
	}
}

func generate(inPath, outPath string) error {
	base, err := dataset.Load(inPath)
	if err != nil {
		return err
	}
	var out []dataset.Pair
	for _, p := range base {
		lang := "en"
		if p.Category == "language_switch" {
			lang = langOf(p.B)
			if lang == "" || lang == "en" {
				return fmt.Errorf("%s: cannot label the language of %q", p.ID, p.B)
			}
		}
		p.Answer = answers[lang]
		p.AnswerLang = lang
		out = append(out, p)
	}
	out = append(out, positives()...)

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, p := range out {
		if err := enc.Encode(p); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "genv2: wrote %d pairs to %s\n", len(out), outPath)
	return nil
}

// answers — полный ответ, а не перефраз вопроса: по нему язык определяется
// уверенно, в отличие от запроса в пять слов.
var answers = map[string]string{
	"en": "Open Settings, then Security. The steps below apply to this account only and do not change another workspace.",
	"ru": "Откройте настройки, затем раздел безопасности. Эти шаги относятся только к этому аккаунту и не меняют другое рабочее пространство.",
	"de": "Öffnen Sie die Einstellungen und dann den Bereich Sicherheit. Diese Schritte gelten nur für dieses Konto und ändern keinen anderen Arbeitsbereich.",
	"fr": "Ouvrez les paramètres, puis la section Sécurité. Ces étapes concernent uniquement ce compte et ne modifient pas un autre espace de travail.",
	"es": "Abra la configuración y después la sección de seguridad. Estos pasos se aplican solo a esta cuenta y no cambian otro espacio de trabajo.",
	"ja": "設定を開き、次にセキュリティの項目を開いてください。以下の手順はこのアカウントだけに適用され、別のワークスペースは変更しません。",
	"zh": "打开设置，然后进入安全部分。以下步骤仅适用于此账户，不会更改其他工作区。",
	"pl": "Otwórz ustawienia, a następnie sekcję bezpieczeństwa. Te kroki dotyczą tylko tego konta i nie zmieniają innego obszaru roboczego.",
	"it": "Apri le impostazioni e poi la sezione Sicurezza. Questi passaggi valgono solo per questo account e non modificano un altro spazio di lavoro.",
	"pt": "Abra as configurações e depois a seção de segurança. Estas etapas valem apenas para esta conta e não alteram outro espaço de trabalho.",
	"tr": "Ayarları açın, ardından Güvenlik bölümüne girin. Bu adımlar yalnızca bu hesap için geçerlidir ve başka bir çalışma alanını değiştirmez.",
}

func langOf(s string) string {
	var cyr, kana, han bool
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyr = true
		case unicode.In(r, unicode.Hiragana, unicode.Katakana):
			kana = true
		case unicode.Is(unicode.Han, r):
			han = true
		}
	}
	switch {
	case cyr:
		return "ru"
	case kana:
		return "ja"
	case han:
		return "zh"
	}
	for _, r := range s {
		switch r {
		case 'ğ', 'Ğ', 'ş', 'Ş', 'ı', 'İ', 'ç', 'Ç':
			return "tr"
		case 'ą', 'ę', 'ł', 'ń', 'ś', 'ź', 'ż', 'Ą', 'Ę', 'Ł', 'Ń', 'Ś', 'Ź', 'Ż':
			return "pl"
		case '¿', 'ñ', 'Ñ', 'á', 'é', 'í', 'ó', 'ú':
			return "es"
		case 'ä', 'ö', 'ü', 'Ä', 'Ö', 'Ü', 'ß':
			return "de"
		case 'à', 'è', 'ê', 'œ':
			return "fr"
		case 'ã', 'õ', 'Ã', 'Õ':
			return "pt"
		}
	}
	switch {
	case hasPrefixFold(s, "Wie "), hasPrefixFold(s, "Was "), hasPrefixFold(s, "Wo "):
		return "de"
	case hasPrefixFold(s, "Comment "), hasPrefixFold(s, "Où "):
		return "fr"
	case hasPrefixFold(s, "Come "), hasPrefixFold(s, "Dove "):
		return "it"
	case hasPrefixFold(s, "Como "), hasPrefixFold(s, "Onde "):
		return "pt"
	case hasPrefixFold(s, "Jak "):
		return "pl"
	case hasPrefixFold(s, "Nasıl "):
		return "tr"
	}
	return ""
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && equalFoldASCII(s[:len(prefix)], prefix)
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// positives — взаимозаменяемые пары на каждом языке, который встречается
// в language_switch. Это шаблоны, не ручная вычитка каждой строки.
func positives() []dataset.Pair {
	type pair struct{ a, b string }
	sets := []struct {
		lang string
		para []pair
		form []pair
	}{
		{"ru", []pair{
			{"Как сбросить пароль?", "Как восстановить пароль от аккаунта?"},
			{"Как создать API-ключ?", "Как выпустить новый ключ API?"},
			{"Как отменить подписку?", "Как отключить платную подписку?"},
		}, []pair{
			{"Как включить двухфакторную аутентификацию?", "как включить двухфакторную аутентификацию?"},
			{"Как пригласить коллегу?", "Подскажите, пожалуйста: как пригласить коллегу?"},
		}},
		{"de", []pair{
			{"Wie setze ich mein Passwort zurück?", "Wie kann ich mein Kontopasswort zurücksetzen?"},
			{"Wie erstelle ich einen API-Schlüssel?", "Wie erzeuge ich einen neuen API-Schlüssel?"},
			{"Wie kündige ich mein Abonnement?", "Wie beende ich das kostenpflichtige Abonnement?"},
		}, []pair{
			{"Wie aktiviere ich 2FA?", "wie aktiviere ich 2fa?"},
			{"Wie lade ich ein Teammitglied ein?", "Bitte: Wie lade ich ein Teammitglied ein?"},
		}},
		{"fr", []pair{
			{"Comment réinitialiser mon mot de passe ?", "Comment retrouver le mot de passe du compte ?"},
			{"Comment créer une clé API ?", "Comment générer une nouvelle clé API ?"},
			{"Comment annuler mon abonnement ?", "Comment résilier l'abonnement payant ?"},
		}, []pair{
			{"Comment inviter un collègue ?", "comment inviter un collègue ?"},
			{"Comment activer la double authentification ?", "Pouvez-vous me dire comment activer la double authentification ?"},
		}},
		{"es", []pair{
			{"¿Cómo restablezco mi contraseña?", "¿Cómo recupero la contraseña de la cuenta?"},
			{"¿Cómo creo una clave API?", "¿Cómo genero una nueva clave de API?"},
			{"¿Cómo cancelo la suscripción?", "¿Cómo doy de baja la suscripción de pago?"},
		}, []pair{
			{"¿Cómo activo la autenticación de dos factores?", "¿cómo activo la autenticación de dos factores?"},
			{"¿Cómo invito a un compañero?", "Por favor, ¿cómo invito a un compañero?"},
		}},
		{"ja", []pair{
			{"パスワードをリセットするにはどうすればよいですか？", "アカウントのパスワードを再設定する方法は？"},
			{"APIキーの作成方法は？", "新しいAPIキーを発行するにはどうしますか？"},
			{"サブスクリプションを解約するには？", "有料プランを解約する手順は？"},
		}, []pair{
			{"二次認証を有効にするにはどうすればよいですか？", "すみません、二次認証を有効にするにはどうすればよいですか？"},
			{"同僚を招待するには？", "教えてください。同僚を招待するには？"},
		}},
		{"zh", []pair{
			{"如何重置密码？", "如何找回账户密码？"},
			{"如何创建 API 密钥？", "如何生成新的 API 密钥？"},
			{"如何取消订阅？", "如何退订付费套餐？"},
		}, []pair{
			{"如何启用双因素认证？", "请问，如何启用双因素认证？"},
			{"如何邀请同事？", "请问：如何邀请同事？"},
		}},
		{"pl", []pair{
			{"Jak zresetować hasło?", "Jak odzyskać hasło do konta?"},
			{"Jak utworzyć klucz API?", "Jak wygenerować nowy klucz API?"},
			{"Jak anulować subskrypcję?", "Jak zrezygnować z płatnej subskrypcji?"},
		}, []pair{
			{"Jak zrotować klucz API?", "jak zrotować klucz api?"},
			{"Jak zaprosić współpracownika?", "Proszę: jak zaprosić współpracownika?"},
		}},
		{"it", []pair{
			{"Come reimposto la password?", "Come recupero la password dell'account?"},
			{"Come creo una chiave API?", "Come genero una nuova chiave API?"},
			{"Come annullo l'abbonamento?", "Come disdico l'abbonamento a pagamento?"},
		}, []pair{
			{"Come attivo l'SSO?", "come attivo l'sso?"},
			{"Come invito un collega?", "Per favore: come invito un collega?"},
		}},
		{"pt", []pair{
			{"Como redefino minha senha?", "Como recupero a senha da conta?"},
			{"Como crio uma chave de API?", "Como gero uma nova chave de API?"},
			{"Como cancelo a assinatura?", "Como encerro a assinatura paga?"},
		}, []pair{
			{"Como crio uma chave de API?", "como crio uma chave de api?"},
			{"Como convido um colega?", "Por favor: como convido um colega?"},
		}},
		{"tr", []pair{
			{"Şifremi nasıl sıfırlarım?", "Hesap şifremi nasıl kurtarırım?"},
			{"API anahtarını nasıl oluştururum?", "Yeni bir API anahtarını nasıl üretirim?"},
			{"Aboneliği nasıl iptal ederim?", "Ücretli aboneliği nasıl sonlandırırım?"},
		}, []pair{
			{"Nasıl iki faktörlü kimlik doğrulamayı açarım?", "nasıl iki faktörlü kimlik doğrulamayı açarım?"},
			{"Bir ekip arkadaşını nasıl davet ederim?", "Lütfen: bir ekip arkadaşını nasıl davet ederim?"},
		}},
	}

	var out []dataset.Pair
	for _, set := range sets {
		if answers[set.lang] == "" {
			panic("missing answer for " + set.lang)
		}
		for i, p := range set.para {
			out = append(out, dataset.Pair{
				ID:              fmt.Sprintf("v2-%s-par-%02d", set.lang, i+1),
				Category:        "paraphrase",
				A:               p.a,
				B:               p.b,
				Interchangeable: true,
				Source:          "template",
				Note:            "same-language positive; template, not individually reviewed",
				Answer:          answers[set.lang],
				AnswerLang:      set.lang,
			})
		}
		for i, p := range set.form {
			out = append(out, dataset.Pair{
				ID:              fmt.Sprintf("v2-%s-fmt-%02d", set.lang, i+1),
				Category:        "format_only",
				A:               p.a,
				B:               p.b,
				Interchangeable: true,
				Source:          "template",
				Note:            "same-language positive; template, not individually reviewed",
				Answer:          answers[set.lang],
				AnswerLang:      set.lang,
			})
		}
	}
	return out
}
