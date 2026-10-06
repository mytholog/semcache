# semcache — tooling

MCP servers for this project are declared in `.cursor/mcp.json`: Serena (symbol-level code
navigation and editing), Context7 (current library docs) and Memory Bank (this directory).

- Serena needs `gopls` on PATH, otherwise its Go language server fails to start and every
  symbol tool returns "language server manager is not initialized". Install it with
  `go install golang.org/x/tools/gopls@latest`; the config prepends `$HOME/go/bin` to PATH.
- Context7 works without an API key at a lower rate limit. Set `CONTEXT7_API_KEY` in the
  environment to raise it; the config passes it through when present.
- Memory Bank stores files under `.memory-bank/<project>/` and they are committed on purpose,
  so the project's decisions outlive any single machine. Serena keeps its own memories in
  `.serena/`, which is gitignored.
- Cloud Agents do not read `.cursor/mcp.json`: it is client configuration. The same three
  servers must be added separately in the MCP dropdown at cursor.com/agents, with secrets
  under Cloud Agents > Secrets.
