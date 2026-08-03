import re
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urlsplit

REPO_ROOT = Path(__file__).parents[1]
INLINE_MARKDOWN_LINK = re.compile(r"!?\[[^\]]*]\(([^)\s]+)")


class _LocalLinkParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.destinations: list[str] = []
        self.anchors: set[str] = set()

    def handle_starttag(
        self,
        tag: str,
        attrs: list[tuple[str, str | None]],
    ) -> None:
        values = dict(attrs)
        for attribute in ("href", "src"):
            if destination := values.get(attribute):
                self.destinations.append(destination)
        for attribute in ("id", "name"):
            if anchor := values.get(attribute):
                self.anchors.add(anchor)


def _documentation_files() -> list[Path]:
    return [
        *REPO_ROOT.glob("*.md"),
        *REPO_ROOT.joinpath("docs").rglob("*.md"),
        *REPO_ROOT.joinpath("docs").rglob("*.html"),
    ]


def _destinations(source: Path, content: str) -> tuple[list[str], set[str]]:
    if source.suffix == ".html":
        parser = _LocalLinkParser()
        parser.feed(content)
        return parser.destinations, parser.anchors
    return INLINE_MARKDOWN_LINK.findall(content), set()


def test_inline_local_documentation_links_resolve() -> None:
    documents = _documentation_files()
    anchors_by_path: dict[Path, set[str]] = {}
    destinations_by_path: dict[Path, list[str]] = {}

    for document in documents:
        destinations, anchors = _destinations(document, document.read_text())
        destinations_by_path[document.resolve()] = destinations
        anchors_by_path[document.resolve()] = anchors

    failures: list[str] = []
    for source, destinations in destinations_by_path.items():
        for destination in destinations:
            parsed = urlsplit(destination.strip("<>"))
            if parsed.scheme or parsed.netloc:
                continue

            target = source if not parsed.path else (source.parent / unquote(parsed.path)).resolve()
            if not target.is_relative_to(REPO_ROOT):
                failures.append(
                    f"{source.relative_to(REPO_ROOT)}: outside repository: {destination}"
                )
            elif not target.exists():
                failures.append(f"{source.relative_to(REPO_ROOT)}: missing: {destination}")
            elif parsed.fragment and target.suffix == ".html":
                anchors = anchors_by_path.get(target)
                if anchors is None:
                    _, anchors = _destinations(target, target.read_text())
                    anchors_by_path[target] = anchors
                if unquote(parsed.fragment) not in anchors:
                    failures.append(
                        f"{source.relative_to(REPO_ROOT)}: missing anchor: {destination}"
                    )

    assert failures == []
