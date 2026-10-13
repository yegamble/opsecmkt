"""MkDocs hooks for the documentation site (mkdocs.yml).

The site is built from docs/, but a few guides live at the repository root and
docs link to source files elsewhere in the repository. This hook:

* publishes the root guides in ROOT_PAGES as pages under project/;
* rewrites links to those guides so they point at the published pages;
* rewrites every other link that leaves docs/ to the file on GitHub.
"""

import os
import posixpath
import re

from mkdocs.structure.files import File

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GITHUB = 'https://github.com/yegamble/opsecmkt/blob/main/'
ROOT_PAGES = ('UPGRADING.md', 'SECURITY.md', 'CONTRIBUTING.md')
VIRTUAL_DIR = 'project'

LINK = re.compile(r'(\]\()([^)\s]+)(\))')
FENCE = re.compile(r'^\s*(```|~~~)')


def on_files(files, config):
    for name in ROOT_PAGES:
        with open(os.path.join(REPO, name), encoding='utf-8') as f:
            files.append(File.generated(config, f'{VIRTUAL_DIR}/{name}', content=f.read()))
    return files


def _rewrite(target, page_dir_abs, page_uri, docs_dir):
    if re.match(r'^[a-z][a-z0-9+.-]*:', target, re.I) or target.startswith(('#', '/')):
        return target
    path, hash_, frag = target.partition('#')
    if not path:
        return target
    absolute = os.path.normpath(os.path.join(page_dir_abs, path))
    page_dir_uri = posixpath.dirname(page_uri)
    if absolute.startswith(docs_dir + os.sep):
        site_path = os.path.relpath(absolute, docs_dir).replace(os.sep, '/')
    elif os.path.dirname(absolute) == REPO and os.path.basename(absolute) in ROOT_PAGES:
        site_path = f'{VIRTUAL_DIR}/{os.path.basename(absolute)}'
    elif absolute.startswith(REPO + os.sep):
        return GITHUB + os.path.relpath(absolute, REPO).replace(os.sep, '/') + hash_ + frag
    else:
        return target
    return posixpath.relpath(site_path, page_dir_uri or '.') + hash_ + frag


def on_page_markdown(markdown, page, config, files):
    docs_dir = os.path.abspath(config['docs_dir'])
    if page.file.generated_by:
        page_dir_abs = REPO
    else:
        page_dir_abs = os.path.dirname(page.file.abs_src_path)
    out, fenced = [], False
    for line in markdown.split('\n'):
        if FENCE.match(line):
            fenced = not fenced
        elif not fenced:
            line = LINK.sub(
                lambda m: m.group(1) + _rewrite(m.group(2), page_dir_abs, page.file.src_uri, docs_dir) + m.group(3),
                line,
            )
        out.append(line)
    return '\n'.join(out)
