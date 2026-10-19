# pomoai-cli

`pomoai` generates images and videos through the PomoAi API and is designed
for both people and AI agents (Claude Code, Codex, Cursor, …).

```sh
npm i -g https://github.com/PomoAi-ai/pomoai-cli/releases/latest/download/pomoai-cli.tgz
pomoai login --base-url <API address>     # paste your API key when asked
pomoai models --kind image
pomoai image create --model <model-id> --prompt "a red fox in snow"
```

That tarball bundles the prebuilt binaries for macOS, Linux and Windows
(x64 / arm64). The registry package, once published, instead pulls one
`pomoai-cli-{darwin,linux,win32}-{x64,arm64}` optional dependency.

Documentation: https://github.com/PomoAi-ai/pomoai-cli
