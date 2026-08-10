<p align="center">
  <img src=".github/resources/banner.png" alt="Spynel — Simplicity. Leverage. Quality." width="100%">
</p>

<p align="center">
  <a href="https://agent-zero.ai"><img alt="Website" src="https://img.shields.io/badge/www-agent--zero.ai-0A192F?style=flat&amp;logo=googlechrome&amp;logoColor=white"></a>
  &nbsp;
  <a href="https://discord.gg/B8KZKNsPpj"><img alt="Discord" src="https://img.shields.io/badge/Discord-5865F2?style=flat&amp;logo=discord&amp;logoColor=white"></a>
  &nbsp;
  <a href="https://x.com/Agent0ai"><img alt="X" src="https://img.shields.io/badge/X-000000?style=flat&amp;logo=x&amp;logoColor=white"></a>
  &nbsp;
  <a href="https://www.youtube.com/@AgentZeroFW"><img alt="YouTube" src="https://img.shields.io/badge/YouTube-FF0000?style=flat&amp;logo=youtube&amp;logoColor=white"></a>
  &nbsp;
  <a href="https://github.com/sponsors/agent0ai"><img alt="GitHub Sponsors" src="https://img.shields.io/badge/Sponsors-FF69B4?style=flat&amp;logo=githubsponsors&amp;logoColor=white"></a>
</p>

  <em>Using more agents should not mean spending more time managing agents - that's not leverage.<br> I want to have one communication channel to all my work. One assistant to talk to. One that will do all the management and scaling instead of me.</em>
  <br><em>Jan Tomášek, founder of <a href="https://github.com/agent0ai/agent-zero">Agent Zero</a></em>

<table>
  <tr>
    <td width="50%" valign="middle">
      <h2>One Human, one Chat</h2>
      <p>Talk with one assistant for all of your AI automation. No more switching between chats, projects, and spaces - that is work for agents.</p>
    </td>
    <td width="50%" align="center">
      <img src=".github/resources/readme-one-human-one-chat.webp" alt="One person using a phone connected to one assistant" width="200" style="width: 100%; max-width: 200px; height: auto;">
    </td>
  </tr>
  <tr>
    <td width="50%" align="center">
      <img src=".github/resources/readme-infinite-leverage.webp" alt="One assistant coordinating a network of agents" width="200" style="width: 100%; max-width: 200px; height: auto;">
    </td>
    <td width="50%" valign="middle">
      <h2>Infinite Leverage</h2>
      <p>Hand off projects, tasks, and agent coordination through the assistant you already talk to. It can orchestrate many independent agents on your behalf, so your leverage scales without your coordination overhead.</p>
    </td>
  </tr>
  <tr>
    <td width="50%" valign="middle">
      <h2>Quality Loops</h2>
      <p>Reliable task management and autonomous agentic loops make thorough testing and review part of the workflow—not an afterthought.</p>
    </td>
    <td width="50%" align="center">
      <img src=".github/resources/readme-quality.webp" alt="An assistant coordinating task management, testing, and review" width="200" style="width: 100%; max-width: 200px; height: auto;">
    </td>
  </tr>
</table>



<br><br>

---

# ◉◉ Spynel in a nutshell

- Spynel is a lightweight program with no AI inside.
- Spynel uses your existing Codex, Claude Code and other coding harnesses to do the work for it.
- Spynel provides communication interface for you and orchestration framework for agents. 


**Install and run**
```bash
npm install -g spynel
spynel
```

The idea is **one human → one assistant → ALL of the work**

Spynel has three pillars:

1. **Communication interface** — work through a single terminal chat UI, Telegram or WhatsApp
2. **Markdown task management** — keep tasks, goals, decisions, evidence, and status durable and easily inspectable by any AI harness
3. **Agentic loops** — let agents plan, implement, review, recover, and ask for human judgment when it matters

Spynel leverages coding harnesses rather than competing with them. Its deterministic core coordinates work; Codex, Claude Code or others provide inteligence and execution.

It is for people who want the leverage of several coding agents without personallěy managing every session and handoff. The result should remain simple to operate, clear to inspect, and deliberate about quality.

**Simplicity. Leverage. Quality.**

## Quick start

Install the latest stable release from npm:

```bash
npm install -g spynel
spynel
```

Run `spynel` from the directory you want to initialize as a Spynel workspace. This can be your existing work folder. 

Inside Spynel TUI, type `/configure` for configuration. Supported coding harnesses should be automatically detected and configured, so there's a good chance Spynel will work out of the box with no additional configuration.


## Documentation

- **Start here:** [Getting started and development](docs/getting-started.md)
- **Configure Spynel:** [Configuration](docs/configuration.md)
- **Use the TUI, Telegram, WhatsApp, and voice:** [Communication integrations](docs/integrations.md)
- **Choose a coding harness:** [Harness compatibility](docs/harness-compatibility.md)
- **Coordinate durable work:** [Tasks and goals](docs/tasks-and-goals.md)
- **Understand the system and its security boundaries:** [Architecture](docs/architecture.md) and [provider-canary threat model](docs/provider-canary-threat-model.md)
- **Automate from the terminal:** [CLI and automation](docs/cli.md) and [agent-readable docs](docs/agent-docs.md)
- **Install trusted hooks:** [Extensions](docs/extensions.md)
- **Build and publish releases:** [Releasing and packaging](docs/releasing.md)
- **Diagnose common setup problems:** [Troubleshooting](docs/troubleshooting.md)
- **Read the product principles:** [Product vision](docs/vision.md)

See the [documentation index](docs/README.md) for the complete guide map.
