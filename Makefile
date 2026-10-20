.PHONY: fmt-check fmt lint lint-audit vet mod-tidy go-quality check vuln workflow-lint deadcode secrets-staged hooks-install

fmt-check fmt lint lint-audit vet mod-tidy go-quality check vuln workflow-lint deadcode secrets-staged hooks-install:
	bash scripts/quality.sh $@
