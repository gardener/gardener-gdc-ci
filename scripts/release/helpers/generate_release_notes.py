#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

"""Generates SAP Gardener-compliant release notes for GDC repositories."""

import argparse
import json
import os
import re
import subprocess
import sys
from typing import Dict, List, Optional, Tuple


CATEGORY_HEADINGS: List[Tuple[str, str]] = [
    ("breaking", "## ⚠️ Breaking Changes"),
    ("feature", "## ✨ New Features"),
    ("bugfix", "## 🐛 Bug Fixes"),
    ("doc", "## 📖 Documentation"),
    ("other", "## 🏃 Others"),
]

CATEGORY_ALIASES: Dict[str, str] = {
    "breaking": "breaking",
    "action": "breaking",
    "noteworthy": "other",
    "feature": "feature",
    "enhancement": "feature",
    "bugfix": "bugfix",
    "fix": "bugfix",
    "bug": "bugfix",
    "doc": "doc",
    "documentation": "doc",
    "other": "other",
}

AUDIENCE_ALIASES: Dict[str, str] = {
    "user": "USER",
    "operator": "OPERATOR",
    "developer": "DEVELOPER",
    "dependency": "DEPENDENCY",
}

BOT_AUTHORS = {
    "gardener-ci-robot",
    "dependabot[bot]",
    "dependabot",
    "renovate[bot]",
    "renovate",
    "github-actions[bot]",
    "federated-github-access[bot]",
}

# Matches Gardener fenced blocks:
# ```<category> <audience>
# <note>
# ```
FENCED_NOTE_RE = re.compile(
    r"```[ \t]*([a-zA-Z_-]+)(?:[ \t]+([a-zA-Z_-]+))?[ \t]*\r?\n(.*?)```",
    re.DOTALL,
)

PR_NUMBER_FROM_SUBJECT_RE = re.compile(
    r"(?:Merge pull request #(\d+)\b|\(#(\d+)\)\s*$)"
)


def parse_fenced_release_notes(text: str) -> Tuple[List[Tuple[str, str, str]], bool]:
    """Extracts (category, audience, note_text) tuples from text.

    Returns:
      (notes, saw_explicit_none):
        notes: list of (canonical_category, canonical_audience, note_text)
        saw_explicit_none: True if a release note block explicitly specified NONE or empty.
    """
    if not text:
        return [], False

    notes: List[Tuple[str, str, str]] = []
    saw_explicit_none = False

    for match in FENCED_NOTE_RE.finditer(text):
        raw_cat = (match.group(1) or "").strip().lower()
        raw_aud = (match.group(2) or "").strip().lower()
        body = (match.group(3) or "").strip()

        if raw_cat in ("release-note", "releasenote"):
            cat = "other"
            aud = "OPERATOR"
        elif raw_cat in CATEGORY_ALIASES and (not raw_aud or raw_aud in AUDIENCE_ALIASES):
            cat = CATEGORY_ALIASES[raw_cat]
            aud = AUDIENCE_ALIASES.get(raw_aud, "OPERATOR")
        else:
            continue

        if not body or body.upper() == "NONE":
            saw_explicit_none = True
            continue

        notes.append((cat, aud, body))

    return notes, saw_explicit_none


def infer_from_subject(subject: str) -> Tuple[str, str, str]:
    """Infers (category, audience, clean_subject) from a commit/PR title."""
    cleaned = re.sub(r"\s*\(#\d+\)\s*$", "", subject.strip())
    conv_match = re.match(r"^([a-zA-Z]+)(?:\([^)]+\))?(!)?:\s*(.+)$", cleaned)
    if conv_match:
        prefix = conv_match.group(1).lower()
        is_breaking = bool(conv_match.group(2))
        rest = conv_match.group(3).strip()
        if is_breaking:
            return "breaking", "OPERATOR", rest
        if prefix == "feat":
            return "feature", "OPERATOR", rest
        if prefix == "fix":
            return "bugfix", "OPERATOR", rest
        if prefix in ("doc", "docs"):
            return "doc", "OPERATOR", rest
        return "other", "OPERATOR", cleaned
    return "other", "OPERATOR", cleaned


def is_synthetic_release_commit(subject: str) -> bool:
    """Returns True if commit is an automated VERSION release or bump commit."""
    s = subject.strip()
    if re.match(r"^release v\d+\.\d+\.\d+", s):
        return True
    if re.match(r"^next version:\s*v\d+\.\d+\.\d+", s):
        return True
    return False


GITHUB_LOGIN_RE = re.compile(r"^[A-Za-z0-9-]+(?:\[bot\])?$")


def format_entry(
    audience: str,
    note_text: str,
    author: Optional[str],
    pr_number: Optional[int],
    repo_slug: str,
) -> str:
    """Formats a single Gardener release note bullet line."""
    lines = [line.rstrip() for line in note_text.strip().splitlines()]
    first_line = lines[0] if lines else ""
    suffix_parts: List[str] = []
    if author:
        clean_author = author.lstrip("@").strip()
        if GITHUB_LOGIN_RE.match(clean_author):
            suffix_parts.append(f"by @{clean_author}")
        else:
            suffix_parts.append(f"by {clean_author}")
    if pr_number:
        pr_url = f"https://github.com/{repo_slug}/pull/{pr_number}"
        suffix_parts.append(f"[[#{pr_number}]({pr_url})]")
    suffix = (" " + " ".join(suffix_parts)) if suffix_parts else ""

    if len(lines) == 1:
        return f"- `[{audience}]` {first_line}{suffix}"
    indented_rest = "\n".join(f"  {l}" if l else "" for l in lines[1:])
    return f"- `[{audience}]` {first_line}{suffix}\n{indented_rest}"


def fetch_pr_details(repo_slug: str, pr_number: int, gh_token: Optional[str]) -> Optional[Dict]:
    """Fetches PR metadata via `gh api` if available."""
    env = os.environ.copy()
    if gh_token:
        env["GH_TOKEN"] = gh_token
    try:
        out = subprocess.check_output(
            ["gh", "api", f"repos/{repo_slug}/pulls/{pr_number}"],
            env=env,
            stderr=subprocess.DEVNULL,
            timeout=15,
        )
        return json.loads(out.decode("utf-8"))
    except Exception:
        return None


def fetch_commit_author_login(
    repo_slug: str, commit_sha: str, gh_token: Optional[str]
) -> Optional[str]:
    """Resolves the GitHub login for a direct commit via `gh api` if available."""
    env = os.environ.copy()
    if gh_token:
        env["GH_TOKEN"] = gh_token
    try:
        out = subprocess.check_output(
            ["gh", "api", f"repos/{repo_slug}/commits/{commit_sha}"],
            env=env,
            stderr=subprocess.DEVNULL,
            timeout=15,
        )
        data = json.loads(out.decode("utf-8"))
        return (data.get("author") or {}).get("login")
    except Exception:
        return None


def collect_git_commits(repo_dir: str, from_tag: Optional[str]) -> List[Dict[str, str]]:
    """Returns commits in `<from_tag>..HEAD` (or `HEAD` if from_tag is empty)."""
    rev_range = f"{from_tag}..HEAD" if from_tag else "HEAD"
    fmt = "%H%x1f%an%x1f%s%x1f%b%x1e"
    try:
        out = subprocess.check_output(
            ["git", "-C", repo_dir, "log", "--reverse", f"--format={fmt}", rev_range],
            stderr=subprocess.DEVNULL,
        ).decode("utf-8", errors="replace")
    except subprocess.CalledProcessError:
        return []

    commits: List[Dict[str, str]] = []
    for raw_rec in out.split("\x1e"):
        raw_rec = raw_rec.strip()
        if not raw_rec:
            continue
        parts = raw_rec.split("\x1f")
        if len(parts) < 4:
            continue
        commits.append(
            {
                "sha": parts[0].strip(),
                "author": parts[1].strip(),
                "subject": parts[2].strip(),
                "body": parts[3].strip(),
            }
        )
    return commits


def extract_repository_notes(
    repo_dir: str,
    repo_slug: str,
    from_tag: Optional[str],
    gh_token: Optional[str],
    image_updates: Optional[List[Tuple[str, str, str]]] = None,
    use_github_api: bool = True,
) -> Dict[str, List[str]]:
    """Collects categorized release notes for a repository."""
    categorized: Dict[str, List[str]] = {cat: [] for cat, _ in CATEGORY_HEADINGS}
    seen_prs = set()
    seen_entries = set()

    commits = collect_git_commits(repo_dir, from_tag)
    for commit in commits:
        subject = commit["subject"]
        body = commit["body"]
        if is_synthetic_release_commit(subject):
            continue

        pr_match = PR_NUMBER_FROM_SUBJECT_RE.search(subject)
        pr_number = int(pr_match.group(1) or pr_match.group(2)) if pr_match else None

        if pr_number and pr_number in seen_prs:
            continue
        if pr_number:
            seen_prs.add(pr_number)

        pr_data = (
            fetch_pr_details(repo_slug, pr_number, gh_token)
            if (pr_number and use_github_api)
            else None
        )
        if pr_data:
            author_login = (pr_data.get("user") or {}).get("login") or commit["author"]
            pr_title = pr_data.get("title") or subject
            pr_body = pr_data.get("body") or ""
            notes, saw_none = parse_fenced_release_notes(pr_body)
            if not notes and not saw_none:
                notes, saw_none = parse_fenced_release_notes(body)
        else:
            author_login = (
                fetch_commit_author_login(repo_slug, commit["sha"], gh_token)
                if use_github_api
                else None
            ) or commit["author"]
            pr_title = subject
            notes, saw_none = parse_fenced_release_notes(body)

        if saw_none and not notes:
            continue

        if notes:
            for cat, aud, note_text in notes:
                entry = format_entry(aud, note_text, author_login, pr_number, repo_slug)
                if entry not in seen_entries:
                    seen_entries.add(entry)
                    categorized[cat].append(entry)
            continue

        # No fenced release-note block was present: skip noisy bot dependency commits
        if author_login in BOT_AUTHORS or subject.startswith("Merge pull request #"):
            continue

        cat, aud, clean_title = infer_from_subject(pr_title)
        entry = format_entry(aud, clean_title, author_login, pr_number, repo_slug)
        if entry not in seen_entries:
            seen_entries.add(entry)
            categorized[cat].append(entry)

    if image_updates:
        changed = [
            (name, old_t, new_t)
            for (name, old_t, new_t) in image_updates
            if old_t != new_t and new_t
        ]
        if changed:
            bullet_lines = ["The following container images have been updated:"]
            for name, old_t, new_t in changed:
                old_display = old_t if old_t else "none"
                bullet_lines.append(f"- `{name}`: `{old_display}` -> `{new_t}`")
            entry = format_entry("DEPENDENCY", "\n".join(bullet_lines), None, None, repo_slug)
            categorized["other"].append(entry)

    return categorized


def render_component_section(
    component_name: str,
    version: str,
    categorized_notes: Dict[str, List[str]],
) -> str:
    """Renders `# [<component_name>:<version>]` and non-empty category sections."""
    lines: List[str] = [f"# [{component_name}:{version}]"]
    has_any = False
    for cat_key, heading in CATEGORY_HEADINGS:
        entries = categorized_notes.get(cat_key) or []
        if not entries:
            continue
        has_any = True
        lines.append("")
        lines.append(heading)
        lines.extend(entries)

    if not has_any:
        lines.append("")
        lines.append("## 🏃 Others")
        lines.append("- `[OPERATOR]` Maintenance release with updated build and dependencies.")

    return "\n".join(lines).strip() + "\n"


def render_full_release_notes(
    primary_section: str,
    subcomponent_sections: List[str],
    helm_charts: List[Tuple[str, str]],
    oci_images: List[Tuple[str, str]],
) -> str:
    """Combines primary section, cascaded subcomponent sections, and OCI artifact catalog."""
    blocks: List[str] = [primary_section.strip()]

    for sub in subcomponent_sections:
        if sub and sub.strip():
            blocks.append(sub.strip())

    if helm_charts:
        chart_lines = ["## Helm Charts"]
        for name, ref in sorted(helm_charts, key=lambda x: x[0]):
            chart_lines.append(f"- {name}: `{ref}`")
        blocks.append("\n".join(chart_lines))

    if oci_images:
        image_lines = ["## Container (OCI) Images"]
        for name, ref in sorted(oci_images, key=lambda x: x[0]):
            image_lines.append(f"- {name}: `{ref}`")
        blocks.append("\n".join(image_lines))

    return "\n\n".join(blocks).strip() + "\n"


def parse_kv_pair(raw: str) -> Tuple[str, str]:
    if "=" not in raw:
        raise ValueError(f"Expected NAME=REF format, got: {raw}")
    k, v = raw.split("=", 1)
    return k.strip(), v.strip()


def parse_image_update(raw: str) -> Tuple[str, str, str]:
    parts = raw.split(":")
    if len(parts) != 3:
        raise ValueError(f"Expected IMAGE_NAME:OLD_TAG:NEW_TAG format, got: {raw}")
    return parts[0].strip(), parts[1].strip(), parts[2].strip()


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Generate SAP Gardener-formatted release notes for GDC repositories."
    )
    parser.add_argument("--repo-dir", required=True, help="Path to cloned git repository.")
    parser.add_argument(
        "--repo-slug",
        required=True,
        help="GitHub owner/repo slug (e.g. gardener/gardener-extension-provider-gdc).",
    )
    parser.add_argument(
        "--component-name",
        default="",
        help="OCM component name (defaults to github.com/<repo-slug>).",
    )
    parser.add_argument("--version", required=True, help="Release version tag (e.g. v0.5.4).")
    parser.add_argument(
        "--from-tag",
        default="",
        help="Previous release tag (e.g. v0.5.3). Empty if initial release.",
    )
    parser.add_argument(
        "--image-update",
        action="append",
        default=[],
        help="Optional imagevector update in NAME:OLD_TAG:NEW_TAG format.",
    )
    parser.add_argument(
        "--subcomponent-notes",
        action="append",
        default=[],
        help="Path to subcomponent markdown notes file to cascade into this release.",
    )
    parser.add_argument(
        "--helm-chart",
        action="append",
        default=[],
        help="Helm chart entry in NAME=OCI_REF format.",
    )
    parser.add_argument(
        "--oci-image",
        action="append",
        default=[],
        help="OCI image entry in NAME=OCI_REF format.",
    )
    parser.add_argument(
        "--output",
        required=True,
        help="Output file path for full GitHub Release markdown.",
    )
    parser.add_argument(
        "--subcomponent-output",
        default="",
        help="Optional output file path for component-only section (for parent cascading).",
    )
    args = parser.parse_args()

    component_name = args.component_name or f"github.com/{args.repo_slug}"
    gh_token = os.environ.get("GDC_RELEASE_GITHUB_TOKEN") or os.environ.get("GH_TOKEN")

    image_updates = [parse_image_update(u) for u in args.image_update]
    helm_charts = [parse_kv_pair(c) for c in args.helm_chart]
    oci_images = [parse_kv_pair(i) for i in args.oci_image]

    categorized = extract_repository_notes(
        repo_dir=args.repo_dir,
        repo_slug=args.repo_slug,
        from_tag=args.from_tag or None,
        gh_token=gh_token,
        image_updates=image_updates,
    )

    primary_section = render_component_section(component_name, args.version, categorized)

    if args.subcomponent_output:
        with open(args.subcomponent_output, "w", encoding="utf-8") as f:
            f.write(primary_section)

    subcomponent_sections: List[str] = []
    for path in args.subcomponent_notes:
        if path and os.path.isfile(path):
            with open(path, "r", encoding="utf-8") as f:
                subcomponent_sections.append(f.read())

    full_markdown = render_full_release_notes(
        primary_section=primary_section,
        subcomponent_sections=subcomponent_sections,
        helm_charts=helm_charts,
        oci_images=oci_images,
    )

    with open(args.output, "w", encoding="utf-8") as f:
        f.write(full_markdown)

    return 0


if __name__ == "__main__":
    sys.exit(main())
