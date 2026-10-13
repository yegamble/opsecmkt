# The documentation site (mkdocs.yml) as a static image, served by deploy/docs/compose.yaml behind its own onion
# service. Build context: the repository root. The build stage installs the hash-locked toolchain and runs the same
# strict build as CI's docs job, so a broken link or anchor fails the image build.
FROM python:3.12-slim-bookworm@sha256:34386ef0cb081344d7ec1c103ba398e6e9f64e9ab3a1509accc92a4e24a07258 AS build
WORKDIR /src
COPY requirements-docs.txt ./
RUN python3 -m venv /venv && /venv/bin/pip install --no-cache-dir --require-hashes --no-deps -r requirements-docs.txt
COPY mkdocs.yml README.md UPGRADING.md SECURITY.md CONTRIBUTING.md ./
COPY scripts/mkdocs_hooks.py scripts/
COPY docs docs
RUN NO_MKDOCS_2_WARNING=1 /venv/bin/mkdocs build --site-dir /site

FROM nginxinc/nginx-unprivileged:1.29-alpine@sha256:0c79d56aee561a1d81c63f00eee5fb5fe29279560cdc55e91425133104c7fbe6
COPY deploy/docs/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /site /usr/share/nginx/html
