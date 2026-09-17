#!/usr/bin/env python3
"""Post CFAA sync confirmation to Discord with an AI-generated summary.

Model fallback chain, configured via the OPENROUTER_MODELS environment
variable (comma-separated OpenRouter model IDs). When unset or when every
configured model fails, a template-based summary is used instead.

Requires:
  OPENROUTER_API_KEY  — OpenRouter API key
  DISCORD_WEBHOOK     — Discord webhook URL

The script reads /tmp/cfaa_sync_info.json for sync metadata and generates
a concise AI summary of the changes, then posts a Discord embed.
"""
import json
import os
import sys
import urllib.error
import urllib.request

MAX_TOKENS = 500
TEMPERATURE = 0.1

# Model fallback chain: injected via OPENROUTER_MODELS (comma-separated).
# Empty list => template-based summary fallback.
OPENROUTER_MODELS = [
    m.strip()
    for m in os.environ.get("OPENROUTER_MODELS", "").split(",")
    if m.strip()
]

USER_AGENT = "CFAA-Sync-Bot (https://github.com/full-bars/sn, 1.0)"
OPENROUTER_URL = "https://openrouter.ai/api/v1/chat/completions"


def call_openrouter(model: str, prompt: str, api_key: str) -> str | None:
    """Call OpenRouter with a model. Returns content or None on failure."""
    payload = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": TEMPERATURE,
        "max_tokens": MAX_TOKENS,
    }).encode("utf-8")
    req = urllib.request.Request(
        OPENROUTER_URL,
        data=payload,
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {api_key}",
            "HTTP-Referer": "https://github.com/full-bars/sn",
            "User-Agent": USER_AGENT,
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            result = json.loads(resp.read().decode("utf-8"))
            return result["choices"][0]["message"]["content"].strip()
    except Exception as e:
        print(f"[CFAA-NOTIFY] {model} failed: {e}", file=sys.stderr)
        return None


def generate_ai_summary(info: dict, api_key: str) -> str:
    """Generate a concise AI summary of the CFAA sync changes."""
    v4_delta = info.get("upstream_v4", 0) - info.get("fork_v4", 0)
    v6_delta = info.get("upstream_v6", 0) - info.get("fork_v6", 0)

    prompt = f"""Summarize this CFAA blocklist sync in 2-3 sentences:

Sync ran at: {info.get("timestamp", "unknown")}
Upstream SHA: {info.get("upstream_sha", "unknown")}
IPv4 blocked prefixes: {info.get("fork_v4", 0)} -> {info.get("upstream_v4", 0)} (delta: {v4_delta:+d})
IPv6 blocked prefixes: {info.get("fork_v6", 0)} -> {info.get("upstream_v6", 0)} (delta: {v6_delta:+d})
Sync needed: {info.get("sync_needed", False)}
Branch: {info.get("branch", "N/A")}
PR: {info.get("pr_url", "N/A")} or already existed

Keep it factual and concise — no emojis, no fluff."""

    for model in OPENROUTER_MODELS:
        summary = call_openrouter(model, prompt, api_key)
        if summary and len(summary) >= 20:
            print(f"[CFAA-NOTIFY] Summary via {model}")
            return summary

    # Fallback to template-based summary (no AI)
    print("[CFAA-NOTIFY] All AI models failed, using template summary")
    action = "synced" if info.get("sync_needed") else "checked, no changes needed"
    return (
        f"CFAA blocklist sync {action} at {info.get('timestamp', 'unknown')}. "
        f"IPv4: {info.get('fork_v4', 0)}->{info.get('upstream_v4', 0)} ({v4_delta:+d}), "
        f"IPv6: {info.get('fork_v6', 0)}->{info.get('upstream_v6', 0)} ({v6_delta:+d}). "
        f"Upstream SHA: {info.get('upstream_sha', 'unknown')}"
    )


def post_to_discord(webhook: str, info: dict, summary: str) -> None:
    """Post a Discord embed with the sync confirmation."""
    sync_needed = info.get("sync_needed", False)
    title = "🛰️ CFAA Blocklist Sync Completed" if sync_needed else "✅ CFAA Blocklist Sync Checked"
    color = 0x5865F2  # blurple

    pr_url = info.get("pr_url", "")
    valid_pr_url = pr_url if (isinstance(pr_url, str) and pr_url.startswith("http")) else None
    pr_field = {"name": "Pull Request", "value": valid_pr_url or "N/A", "inline": True}

    fields = [
        {"name": "Action", "value": "synced" if sync_needed else "checked (no changes)", "inline": True},
        {"name": "Upstream SHA", "value": info.get("upstream_sha", "N/A"), "inline": True},
        {"name": "IPv4 Prefixes", "value": f"{info.get('fork_v4', 0)} -> {info.get('upstream_v4', 0)}", "inline": True},
        {"name": "IPv6 Prefixes", "value": f"{info.get('fork_v6', 0)} -> {info.get('upstream_v6', 0)}", "inline": True},
        pr_field,
        {"name": "AI Summary", "value": summary, "inline": False},
    ]

    embed = {
        "title": title,
        "description": "Automated CFAA blocklist sync from urnetwork/connect upstream.",
        "color": color,
        "fields": fields,
        "footer": {"text": "URNetwork CFAA Sync • Automated"},
    }
    if valid_pr_url:
        embed["url"] = valid_pr_url

    payload = json.dumps({"embeds": [embed]})

    req = urllib.request.Request(
        webhook,
        data=payload.encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "User-Agent": USER_AGENT,
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            print(f"[CFAA-NOTIFY] Discord POST: HTTP {resp.status}")
    except Exception as e:
        print(f"[CFAA-NOTIFY] Discord POST failed: {e}", file=sys.stderr)
        # Write payload to file as fallback
        with open("/tmp/cfaa_discord_payload.json", "w") as f:
            f.write(payload)


def main() -> int:
    info_path = "/tmp/cfaa_sync_info.json"

    # Read sync info written by previous workflow steps
    if os.path.exists(info_path):
        with open(info_path, "r") as f:
            info = json.load(f)
    else:
        info = {}

    api_key = os.environ.get("OPENROUTER_API_KEY", "")
    webhook = os.environ.get("DISCORD_WEBHOOK", "")

    summary = ""
    if api_key:
        summary = generate_ai_summary(info, api_key)
    else:
        print("[CFAA-NOTIFY] OPENROUTER_API_KEY not set, using template summary")
        v4_delta = info.get("upstream_v4", 0) - info.get("fork_v4", 0)
        v6_delta = info.get("upstream_v6", 0) - info.get("fork_v6", 0)
        action = "synced" if info.get("sync_needed") else "checked, no changes needed"
        summary = (
            f"CFAA blocklist {action}. IPv4: {info.get('fork_v4', 0)}->"
            f"{info.get('upstream_v4', 0)} ({v4_delta:+d}), IPv6: "
            f"{info.get('fork_v6', 0)}->{info.get('upstream_v6', 0)} ({v6_delta:+d}). "
            f"Upstream SHA: {info.get('upstream_sha', 'unknown')}"
        )

    # Always post confirmation (even if sync wasn't needed)
    if webhook:
        post_to_discord(webhook, info, summary)
    else:
        print("[CFAA-NOTIFY] DISCORD_WEBHOOK not set, skipping notification")

    return 0


if __name__ == "__main__":
    sys.exit(main())
