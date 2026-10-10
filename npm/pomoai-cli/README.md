# pomoai-cli

`pomoai` generates images and videos through the PomoAi API and is designed
for both people and AI agents (Claude Code, Codex, Cursor, …).

```sh
npm i -g pomoai-cli
export POMOAI_API_KEY=sk-...        # or run `pomoai login`
pomoai image create --model gpt-image-1 --prompt "a red fox in snow" --json
```

The prebuilt binary comes from a platform package
(`pomoai-cli-{darwin,linux,win32}-{x64,arm64}`) installed as an optional
dependency. If optional dependencies are disabled, use the install script
instead:

```sh
curl -fsSL https://raw.githubusercontent.com/PomoAi-ai/pomoai-cli/main/install.sh | sh
```

Documentation: https://github.com/PomoAi-ai/pomoai-cli
