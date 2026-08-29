// Command genv1 собирает bench/dataset/v1.jsonl: 80 ручных пар из pilot.jsonl
// плюс контролируемые минимальные пары из шаблонов. Шаблонные пары не
// выдаются за вычитанные вручную — см. bench/dataset/LABELING.md.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/mytholog/semcache/internal/dataset"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	pilotPath := filepath.Join(root, "bench/dataset/pilot.jsonl")
	outPath := filepath.Join(root, "bench/dataset/v1.jsonl")
	if err := generate(pilotPath, outPath); err != nil {
		fmt.Fprintf(os.Stderr, "genv1: %v\n", err)
		os.Exit(1)
	}
}

func generate(pilotPath, outPath string) error {
	pilot, err := dataset.Load(pilotPath)
	if err != nil {
		return err
	}
	for i := range pilot {
		if pilot[i].Source == "" {
			pilot[i].Source = "hand"
		}
	}

	seenID := make(map[string]struct{}, 1024)
	seenText := make(map[string]struct{}, 1024)
	var out []dataset.Pair
	accept := func(p dataset.Pair) {
		key := p.A + "\x00" + p.B
		rev := p.B + "\x00" + p.A
		if _, ok := seenID[p.ID]; ok {
			return
		}
		if _, ok := seenText[key]; ok {
			return
		}
		if _, ok := seenText[rev]; ok {
			return
		}
		if p.A == p.B {
			return
		}
		seenID[p.ID] = struct{}{}
		seenText[key] = struct{}{}
		out = append(out, p)
	}

	for _, p := range pilot {
		accept(p)
	}
	for _, p := range generated() {
		accept(p)
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	if err := dataset.WriteJSONL(outPath, out); err != nil {
		return err
	}

	counts := map[string]int{}
	var hand int
	for _, p := range out {
		counts[p.Category]++
		if p.HumanAuthored {
			hand++
		}
	}
	fmt.Printf("wrote %s: %d pairs (%d hand-written)\n", outPath, len(out), hand)
	for _, cat := range []string{
		"negation", "entity_swap", "numeric", "temporal", "scope",
		"paraphrase", "format_only", "language_switch",
	} {
		fmt.Printf("  %-16s %d\n", cat, counts[cat])
	}
	if len(out) < 600 || len(out) > 1000 {
		return fmt.Errorf("v1 size %d is outside the 600–1000 target", len(out))
	}
	return nil
}

func generated() []dataset.Pair {
	g := &gen{n: map[string]int{}}
	g.negation()
	g.entitySwap()
	g.numeric()
	g.temporal()
	g.scope()
	g.paraphrase()
	g.formatOnly()
	g.languageSwitch()
	return g.out
}

type gen struct {
	out []dataset.Pair
	n   map[string]int
}

func (g *gen) add(category, a, b string) {
	g.n[category]++
	g.out = append(g.out, dataset.Pair{
		ID:              fmt.Sprintf("gen-%s-%03d", short[category], g.n[category]),
		Category:        category,
		A:               a,
		B:               b,
		Interchangeable: dataset.Categories[category],
		HumanAuthored:   false,
		Source:          "template",
	})
}

var short = map[string]string{
	"negation":        "neg",
	"entity_swap":     "ent",
	"numeric":         "num",
	"temporal":        "tmp",
	"scope":           "scp",
	"paraphrase":      "par",
	"format_only":     "fmt",
	"language_switch": "lng",
}

func (g *gen) negation() {
	objects := []string{
		"the free plan", "the starter plan", "the enterprise contract",
		"the public SDK", "the mobile app", "the inbound webhook",
		"the cluster agent", "the partner API",
	}
	features := []string{
		"SSO", "SCIM", "audit logs", "IP allowlisting", "custom domains",
		"HIPAA mode", "priority support", "the admin API", "SAML", "webhooks",
	}
	for _, obj := range objects {
		for _, f := range features {
			g.add("negation",
				"Does "+obj+" include "+f+"?",
				"Does "+obj+" exclude "+f+"?")
		}
	}
	subjects := []string{"the agent", "the worker", "the gateway", "the CLI"}
	for _, s := range subjects {
		g.add("negation", "Can I deploy "+s+" without a license key?", "Can I deploy "+s+" with a license key?")
		g.add("negation", "Should I enable debug logs on "+s+" in production?", "Should I disable debug logs on "+s+" in production?")
		g.add("negation", "Is the default admin port on "+s+" open to the internet?", "Is the default admin port on "+s+" closed to the internet?")
	}
}

func (g *gen) entitySwap() {
	type pair struct{ a, b string }
	jurisdictions := []pair{
		{"EU customers", "US customers"},
		{"UK customers", "AU customers"},
		{"German tenants", "French tenants"},
		{"California accounts", "Texas accounts"},
	}
	for _, p := range jurisdictions {
		g.add("entity_swap", "What is the refund policy for "+p.a+"?", "What is the refund policy for "+p.b+"?")
		g.add("entity_swap", "Where is customer data stored for "+p.a+"?", "Where is customer data stored for "+p.b+"?")
		g.add("entity_swap", "What are support hours for "+p.a+"?", "What are support hours for "+p.b+"?")
	}

	envs := []pair{
		{"staging", "production"},
		{"dev", "production"},
		{"sandbox", "live"},
		{"preview", "production"},
	}
	for _, p := range envs {
		g.add("entity_swap", "How do I rotate the API key for the "+p.a+" environment?", "How do I rotate the API key for the "+p.b+" environment?")
		g.add("entity_swap", "Where are the webhook secrets for "+p.a+"?", "Where are the webhook secrets for "+p.b+"?")
		g.add("entity_swap", "How do I tail logs in "+p.a+"?", "How do I tail logs in "+p.b+"?")
	}

	vendors := []pair{
		{"Redis", "Postgres"},
		{"Stripe", "PayPal"},
		{"GitHub", "GitLab"},
		{"Slack", "Teams"},
		{"S3", "GCS"},
		{"Okta", "Azure AD"},
		{"Datadog", "Grafana Cloud"},
		{"Cloudflare", "Fastly"},
		{"Twilio", "MessageBird"},
		{"SendGrid", "Postmark"},
	}
	for _, p := range vendors {
		g.add("entity_swap", "Where do I set the "+p.a+" connection string?", "Where do I set the "+p.b+" connection string?")
		g.add("entity_swap", "How do I verify a "+p.a+" webhook signature?", "How do I verify a "+p.b+" webhook signature?")
		g.add("entity_swap", "What env var holds the "+p.a+" API token?", "What env var holds the "+p.b+" API token?")
	}

	regions := []pair{
		{"us-east-1", "eu-west-1"},
		{"us-west-2", "ap-southeast-1"},
		{"eu-central-1", "sa-east-1"},
		{"af-south-1", "ap-northeast-1"},
	}
	for _, p := range regions {
		g.add("entity_swap", "What is the outbound IP range for "+p.a+"?", "What is the outbound IP range for "+p.b+"?")
		g.add("entity_swap", "Which availability zones does "+p.a+" use?", "Which availability zones does "+p.b+" use?")
		g.add("entity_swap", "What is the S3 endpoint in "+p.a+"?", "What is the S3 endpoint in "+p.b+"?")
	}

	hosts := []pair{
		{"app.example.com", "api.example.com"},
		{"admin.example.com", "app.example.com"},
		{"status.example.net", "docs.example.net"},
		{"cdn.acme.io", "origin.acme.io"},
	}
	for _, p := range hosts {
		g.add("entity_swap", "Where is the TLS certificate for "+p.a+"?", "Where is the TLS certificate for "+p.b+"?")
		g.add("entity_swap", "How do I point DNS for "+p.a+"?", "How do I point DNS for "+p.b+"?")
	}

	orgs := []pair{
		{"Acme Corp", "Globex Inc"},
		{"Initech", "Umbrella Ltd"},
		{"the organization workspace", "the personal workspace"},
		{"the parent account", "the child account"},
	}
	for _, p := range orgs {
		g.add("entity_swap", "Who is the billing contact for "+p.a+"?", "Who is the billing contact for "+p.b+"?")
		g.add("entity_swap", "How do I invite a user to "+p.a+"?", "How do I invite a user to "+p.b+"?")
	}
}

func (g *gen) numeric() {
	type pair struct{ a, b string }
	plans := []pair{
		{"100k", "10k"}, {"1M", "100k"}, {"5k", "50k"}, {"250k", "25k"},
		{"2M", "200k"}, {"75k", "7.5k"},
	}
	for _, p := range plans {
		g.add("numeric", "What is the rate limit on the "+p.a+" plan?", "What is the rate limit on the "+p.b+" plan?")
		g.add("numeric", "How many events per month does the "+p.a+" plan include?", "How many events per month does the "+p.b+" plan include?")
		g.add("numeric", "How many seats does the "+p.a+" plan include?", "How many seats does the "+p.b+" plan include?")
	}
	windows := []pair{
		{"30-day", "90-day"}, {"7-day", "30-day"}, {"14-day", "60-day"}, {"1-year", "30-day"},
	}
	for _, p := range windows {
		g.add("numeric", "How long is data retained on the "+p.a+" tier?", "How long is data retained on the "+p.b+" tier?")
	}
	timeouts := []pair{
		{"30 second", "300 second"}, {"5 second", "50 second"}, {"15 second", "60 second"}, {"2 minute", "10 minute"},
	}
	for _, p := range timeouts {
		g.add("numeric", "What is the request timeout on the "+p.a+" setting?", "What is the request timeout on the "+p.b+" setting?")
	}
	sizes := []pair{
		{"4 MB", "16 MB"}, {"1 MB", "10 MB"}, {"32 MB", "128 MB"}, {"512 KB", "5 MB"},
	}
	for _, p := range sizes {
		g.add("numeric", "What is the max upload size on the "+p.a+" limit?", "What is the max upload size on the "+p.b+" limit?")
	}
	prices := []pair{
		{"$29", "$99"}, {"$9", "$49"}, {"$199", "$499"}, {"$15", "$150"},
	}
	for _, p := range prices {
		g.add("numeric", "What is included in the "+p.a+" plan?", "What is included in the "+p.b+" plan?")
	}
	slas := []pair{
		{"99.9%", "99.99%"}, {"99.5%", "99.9%"}, {"99.95%", "99.99%"},
	}
	for _, p := range slas {
		g.add("numeric", "What SLA is guaranteed on the "+p.a+" tier?", "What SLA is guaranteed on the "+p.b+" tier?")
	}
	cpus := []pair{
		{"2-core", "8-core"}, {"4-core", "16-core"}, {"1-core", "4-core"}, {"8-core", "32-core"},
	}
	for _, p := range cpus {
		g.add("numeric", "How many vCPUs are allocated on the "+p.a+" instance?", "How many vCPUs are allocated on the "+p.b+" instance?")
	}
	conns := []pair{
		{"5-conn", "50-conn"}, {"10-conn", "100-conn"}, {"25-conn", "250-conn"}, {"2-conn", "20-conn"},
	}
	for _, p := range conns {
		g.add("numeric", "How many concurrent connections does the "+p.a+" plan allow?", "How many concurrent connections does the "+p.b+" plan allow?")
	}
	batches := []pair{
		{"100-item", "1000-item"}, {"50-item", "500-item"}, {"10-item", "100-item"}, {"256-item", "1024-item"},
	}
	for _, p := range batches {
		g.add("numeric", "How large is the batch size on the "+p.a+" setting?", "How large is the batch size on the "+p.b+" setting?")
	}
	ports := []pair{
		{"8080", "8081"}, {"443", "8443"}, {"5432", "6432"}, {"6379", "6380"},
	}
	for _, p := range ports {
		g.add("numeric", "Which port does the service listen on by default, "+p.a+" or something else?", "Which port does the service listen on by default, "+p.b+" or something else?")
	}
	tokens := []pair{
		{"4k", "8k"}, {"8k", "32k"}, {"16k", "128k"}, {"128k", "1M"},
	}
	for _, p := range tokens {
		g.add("numeric", "What is the context window on the "+p.a+" model?", "What is the context window on the "+p.b+" model?")
	}
}

func (g *gen) temporal() {
	releases := [][2]string{
		{"3.1", "3.2"}, {"4.0", "5.0"}, {"2.12", "2.13"}, {"10.11", "10.12"},
		{"2024.1", "2024.2"}, {"1.0", "1.1"}, {"0.9", "1.0"}, {"6.5", "6.6"},
		{"11.0", "12.0"}, {"8.2", "8.3"},
	}
	for _, p := range releases {
		g.add("temporal", "What changed in release "+p[0]+"?", "What changed in release "+p[1]+"?")
		g.add("temporal", "Which endpoints were deprecated in "+p[0]+"?", "Which endpoints were deprecated in "+p[1]+"?")
	}
	quarters := [][2]string{
		{"Q1 2026", "Q2 2026"}, {"Q3 2025", "Q4 2025"}, {"Q4 2025", "Q1 2026"}, {"H1 2026", "H2 2026"},
	}
	for _, p := range quarters {
		g.add("temporal", "Which features shipped in "+p[0]+"?", "Which features shipped in "+p[1]+"?")
	}
	years := [][2]string{{"2024", "2025"}, {"2025", "2026"}, {"2023", "2024"}}
	for _, p := range years {
		g.add("temporal", "What is the pricing as of "+p[0]+"?", "What is the pricing as of "+p[1]+"?")
		g.add("temporal", "Which TLS ciphers were allowed in "+p[0]+"?", "Which TLS ciphers were allowed in "+p[1]+"?")
	}
	months := [][2]string{
		{"January", "February"}, {"March", "April"}, {"June", "July"}, {"November", "December"},
	}
	for _, p := range months {
		g.add("temporal", "Where is "+p[0]+"'s invoice?", "Where is "+p[1]+"'s invoice?")
	}
	rel := [][2]string{
		{"before the database migration", "after the database migration"},
		{"before the SSO cutoff", "after the SSO cutoff"},
		{"before the April cutoff", "after the April cutoff"},
		{"before the price change", "after the price change"},
		{"before the schema freeze", "after the schema freeze"},
		{"last week", "this week"},
		{"yesterday", "today"},
		{"last month", "this month"},
		{"last quarter", "this quarter"},
		{"last year", "this year"},
	}
	for _, p := range rel {
		g.add("temporal", "What should I do "+p[0]+"?", "What should I do "+p[1]+"?")
		g.add("temporal", "What broke "+p[0]+"?", "What broke "+p[1]+"?")
	}
	changelogs := [][2]string{
		{"yesterday's changelog", "today's changelog"},
		{"last week's changelog", "this week's changelog"},
		{"the 3.1 changelog", "the 3.2 changelog"},
		{"the May changelog", "the June changelog"},
	}
	for _, p := range changelogs {
		g.add("temporal", "What is in "+p[0]+"?", "What is in "+p[1]+"?")
	}
	apis := [][2]string{{"v1", "v2"}, {"v2", "v3"}, {"v3", "v4"}, {"legacy", "current"}}
	for _, p := range apis {
		g.add("temporal", "How do I call the "+p[0]+" API?", "How do I call the "+p[1]+" API?")
		g.add("temporal", "What auth header does the "+p[0]+" API expect?", "What auth header does the "+p[1]+" API expect?")
	}
}

func (g *gen) scope() {
	type pair struct{ a, b string }
	accounts := []pair{
		{"trial accounts", "enterprise accounts"},
		{"free accounts", "paid accounts"},
		{"student accounts", "business accounts"},
		{"internal staff accounts", "customer accounts"},
	}
	for _, p := range accounts {
		g.add("scope", "Does the audit log apply to "+p.a+"?", "Does the audit log apply to "+p.b+"?")
		g.add("scope", "Are refunds available on "+p.a+"?", "Are refunds available on "+p.b+"?")
	}
	roles := []pair{
		{"admin users", "read-only users"},
		{"owners", "members"},
		{"billing admins", "developers"},
		{"service accounts", "human users"},
	}
	for _, p := range roles {
		g.add("scope", "Is MFA required for "+p.a+"?", "Is MFA required for "+p.b+"?")
		g.add("scope", "Do "+p.a+" have permission to delete the workspace?", "Do "+p.b+" have permission to delete the workspace?")
	}
	repos := []pair{
		{"public repositories", "private repositories"},
		{"internal packages", "public packages"},
		{"open-source projects", "commercial projects"},
	}
	for _, p := range repos {
		g.add("scope", "Can I use this feature on "+p.a+"?", "Can I use this feature on "+p.b+"?")
	}
	regions := []pair{
		{"EU-only workspaces", "global workspaces"},
		{"US-only tenants", "multi-region tenants"},
	}
	for _, p := range regions {
		g.add("scope", "Does data residency apply to "+p.a+"?", "Does data residency apply to "+p.b+"?")
	}
	maturity := []pair{
		{"the beta feature", "the GA feature"},
		{"preview APIs", "stable APIs"},
		{"experimental flags", "default settings"},
	}
	for _, p := range maturity {
		g.add("scope", "Is "+p.a+" covered by the SLA?", "Is "+p.b+" covered by the SLA?")
	}
	keys := []pair{
		{"sandbox API keys", "live API keys"},
		{"test webhooks", "production webhooks"},
	}
	for _, p := range keys {
		g.add("scope", "Can "+p.a+" charge live cards?", "Can "+p.b+" charge live cards?")
	}
	when := []pair{
		{"weekends", "weekdays"},
		{"business hours", "nights"},
		{"public holidays", "working days"},
	}
	for _, p := range when {
		g.add("scope", "Does on-call paging fire on "+p.a+"?", "Does on-call paging fire on "+p.b+"?")
		g.add("scope", "Are deploys allowed on "+p.a+"?", "Are deploys allowed on "+p.b+"?")
	}
	apis := []pair{
		{"the internal API", "the customer-facing API"},
		{"the partner API", "the public API"},
		{"the admin API", "the end-user API"},
	}
	for _, p := range apis {
		g.add("scope", "Is this endpoint available on "+p.a+"?", "Is this endpoint available on "+p.b+"?")
		g.add("scope", "Does rate limiting apply to "+p.a+"?", "Does rate limiting apply to "+p.b+"?")
	}
	seats := []pair{
		{"monthly plans", "annual plans"},
		{"seat-based plans", "usage-based plans"},
		{"self-serve plans", "contracted plans"},
	}
	for _, p := range seats {
		g.add("scope", "Are refunds available on "+p.a+"?", "Are refunds available on "+p.b+"?")
		g.add("scope", "Is SSO included on "+p.a+"?", "Is SSO included on "+p.b+"?")
	}
	devices := []pair{
		{"iOS", "Android"},
		{"the web app", "the desktop app"},
		{"the CLI", "the GUI"},
	}
	for _, p := range devices {
		g.add("scope", "Is push notification setup documented for "+p.a+"?", "Is push notification setup documented for "+p.b+"?")
		g.add("scope", "Does offline mode work on "+p.a+"?", "Does offline mode work on "+p.b+"?")
	}
}

func (g *gen) paraphrase() {
	how := []string{
		"reset my password", "create a new API key", "cancel my subscription",
		"rotate an expired secret", "invite a teammate", "connect a custom domain",
		"enable two-factor authentication", "export my logs", "delete my account",
		"raise the API quota", "configure a webhook", "set up SSO",
		"rotate a TLS certificate", "add a payment method", "close a support ticket",
		"restore a backup", "pause billing", "change the workspace name",
		"revoke a session", "turn on audit logging",
	}
	for _, a := range how {
		g.add("paraphrase", "How do I "+a+"?", "What is the process for "+a+"?")
		g.add("paraphrase", "How do I "+a+"?", "How can I "+a+"?")
	}
	where := []string{
		"the audit log", "last month's invoice", "the status page",
		"the webhook secret", "the data processing addendum", "the rate limit dashboard",
		"the IP allowlist", "the SSO metadata XML",
	}
	for _, w := range where {
		g.add("paraphrase", "Where is "+w+"?", "How do I find "+w+"?")
		g.add("paraphrase", "Where can I get "+w+"?", "How do I access "+w+"?")
	}
	why := []string{
		"my webhook is failing", "my API key was rejected", "SSO login loops",
		"the invoice is missing tax", "deployments are stuck queued",
		"the domain is not verifying", "webhooks are delayed",
		"MFA codes are rejected", "the export job never finishes",
		"seats are not freeing after offboarding",
	}
	for _, w := range why {
		g.add("paraphrase", "Why is it that "+w+"?", "What causes the issue when "+w+"?")
		g.add("paraphrase", "Why "+w+"?", "What is the usual reason "+w+"?")
	}
}

func (g *gen) formatOnly() {
	bases := []string{
		"How do I enable SSO?",
		"How do I rotate an API key?",
		"What is the refund policy?",
		"How do I export logs?",
		"Where is the status page?",
		"How do I delete my account?",
		"List the supported webhook events.",
		"What are the required headers?",
		"How do I enable 2FA?",
		"How do I connect a custom domain?",
		"Where can I download last month's invoice?",
		"How do I cancel my subscription?",
		"What is the rate limit on the starter plan?",
		"How do I invite a teammate?",
		"How do I configure a webhook?",
		"List available regions.",
		"How do I restore a backup?",
		"How do I revoke a session?",
		"What SLA is guaranteed on the enterprise tier?",
		"How do I set up IP allowlisting?",
		"How do I pause billing?",
		"How do I change the workspace name?",
		"How do I turn on audit logging?",
		"How do I rotate a TLS certificate?",
		"How do I add a payment method?",
		"How do I close a support ticket?",
		"How do I set up SCIM?",
		"Where is the data processing addendum?",
		"How do I tail production logs?",
		"What is included in the $29 plan?",
	}
	type variant func(string) string
	variants := []variant{
		func(s string) string { return "Could you please " + uncapitalize(s) },
		func(s string) string { return lower(s) },
		func(s string) string { return s + "???" },
		func(s string) string { return s + " Thanks!" },
		func(s string) string { return "hey, " + uncapitalize(s) },
		func(s string) string { return "pls " + uncapitalize(s) },
		func(s string) string { return stretchSpaces(s) },
		func(s string) string { return "**" + s + "**" },
		func(s string) string { return s + " 😊" },
		func(s string) string { return "Hi — " + s },
	}
	for i, base := range bases {
		for k := 0; k < 3; k++ {
			v := variants[(i+k)%len(variants)]
			alt := v(base)
			if alt != base {
				g.add("format_only", base, alt)
			}
		}
	}
}

func (g *gen) languageSwitch() {
	pairs := [][2]string{
		{"How do I enable two-factor authentication?", "Как включить двухфакторную аутентификацию?"},
		{"How do I reset my password?", "Как сбросить пароль?"},
		{"What is the refund policy?", "Какая политика возврата средств?"},
		{"How do I create an API key?", "Как создать API-ключ?"},
		{"Why is my webhook failing?", "Почему не доставляется вебхук?"},
		{"How do I cancel my subscription?", "Как отменить подписку?"},
		{"How do I rotate an API key?", "Как ротировать API-ключ?"},
		{"How do I delete my account?", "Как удалить аккаунт?"},
		{"How do I enable SSO?", "Как включить SSO?"},
		{"How do I export logs?", "Как выгрузить логи?"},
		{"Where is the status page?", "Где статус-страница?"},
		{"How do I invite a teammate?", "Как пригласить коллегу?"},
		{"How do I connect a custom domain?", "Как подключить свой домен?"},
		{"How do I restore a backup?", "Как восстановить бэкап?"},
		{"How do I raise the API quota?", "Как увеличить квоту API?"},
		{"What is the rate limit on the starter plan?", "Какой rate limit на стартовом тарифе?"},
		{"How do I configure a webhook?", "Как настроить вебхук?"},
		{"How do I set up IP allowlisting?", "Как настроить IP allowlist?"},
		{"Where can I download last month's invoice?", "Где скачать счёт за прошлый месяц?"},
		{"How do I revoke a session?", "Как отозвать сессию?"},
		{"How do I pause billing?", "Как приостановить оплату?"},
		{"How do I change the workspace name?", "Как переименовать воркспейс?"},
		{"How do I turn on audit logging?", "Как включить журнал аудита?"},
		{"How do I rotate a TLS certificate?", "Как обновить TLS-сертификат?"},
		{"How do I add a payment method?", "Как добавить способ оплаты?"},
		{"List the supported webhook events.", "Перечислите поддерживаемые события вебхуков."},
		{"What are the required headers?", "Какие заголовки обязательны?"},
		{"List available regions.", "Перечислите доступные регионы."},
		{"What SLA is guaranteed on the enterprise tier?", "Какой SLA на корпоративном тарифе?"},
		{"How do I close a support ticket?", "Как закрыть тикет поддержки?"},
		{"How do I enable 2FA?", "Wie aktiviere ich 2FA?"},
		{"How do I rotate an API key?", "Wie rotiere ich einen API-Schlüssel?"},
		{"What is the refund policy?", "Wie lautet die Rückerstattungsrichtlinie?"},
		{"How do I cancel my subscription?", "Wie kündige ich mein Abonnement?"},
		{"Where is the status page?", "Wo ist die Statusseite?"},
		{"How do I export logs?", "Wie exportiere ich Logs?"},
		{"How do I enable SSO?", "Comment activer le SSO ?"},
		{"How do I reset my password?", "Comment réinitialiser mon mot de passe ?"},
		{"How do I delete my account?", "Comment supprimer mon compte ?"},
		{"Where can I download last month's invoice?", "Où télécharger la facture du mois dernier ?"},
		{"How do I create an API key?", "Comment créer une clé API ?"},
		{"How do I invite a teammate?", "Comment inviter un collègue ?"},
		{"How do I enable two-factor authentication?", "¿Cómo activo la autenticación de dos factores?"},
		{"How do I reset my password?", "¿Cómo restablezco mi contraseña?"},
		{"What is the refund policy?", "¿Cuál es la política de reembolso?"},
		{"How do I cancel my subscription?", "¿Cómo cancelo la suscripción?"},
		{"How do I delete my account?", "¿Cómo elimino mi cuenta?"},
		{"How do I create an API key?", "¿Cómo creo una clave API?"},
		{"How do I enable two-factor authentication?", "二次認証を有効にするにはどうすればよいですか？"},
		{"How do I reset my password?", "パスワードをリセットするにはどうすればよいですか？"},
		{"How do I create an API key?", "APIキーの作成方法は？"},
		{"How do I cancel my subscription?", "サブスクリプションを解約するには？"},
		{"How do I enable two-factor authentication?", "如何启用双因素认证？"},
		{"How do I reset my password?", "如何重置密码？"},
		{"How do I create an API key?", "如何创建 API 密钥？"},
		{"How do I cancel my subscription?", "如何取消订阅？"},
		{"How do I delete my account?", "如何删除账户？"},
		{"How do I export logs?", "如何导出日志？"},
		{"How do I enable SSO?", "如何启用 SSO？"},
		{"Where is the status page?", "状态页在哪里？"},
		{"How do I rotate an API key?", "Jak zrotować klucz API?"},
		{"How do I reset my password?", "Jak zresetować hasło?"},
		{"How do I cancel my subscription?", "Jak anulować subskrypcję?"},
		{"How do I enable SSO?", "Come attivo l'SSO?"},
		{"How do I reset my password?", "Come reimposto la password?"},
		{"How do I delete my account?", "Come elimino l'account?"},
		{"How do I create an API key?", "Como crio uma chave de API?"},
		{"How do I reset my password?", "Como redefino minha senha?"},
		{"How do I cancel my subscription?", "Como cancelo a assinatura?"},
		{"How do I enable two-factor authentication?", "Nasıl iki faktörlü kimlik doğrulamayı açarım?"},
		{"How do I reset my password?", "Şifremi nasıl sıfırlarım?"},
		{"How do I create an API key?", "API anahtarını nasıl oluştururum?"},
	}
	for _, p := range pairs {
		g.add("language_switch", p[0], p[1])
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func uncapitalize(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'A' && b[0] <= 'Z' {
		b[0] += 'a' - 'A'
	}
	return string(b)
}

func stretchSpaces(s string) string {
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		out = append(out, s[i])
		if s[i] == ' ' {
			out = append(out, ' ', ' ')
		}
	}
	return string(out)
}
