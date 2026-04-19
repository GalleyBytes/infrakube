#!/bin/bash
set -euo pipefail

# entrypoint-wrapper.sh
#
# Wraps the compiled entrypoint. For terraform or tofu tasks, extracts the
# requested binary before handing off to the main entrypoint.

TASK="${INFRAKUBE_TASK:-}"

case "${TASK}" in
    init|init-delete|plan|plan-delete|apply|apply-delete)
        if [ -n "${INFRAKUBE_TOFU_VERSION:-}" ]; then
            /opt/tofu/extract-tofu.sh
        elif [ -n "${INFRAKUBE_TF_VERSION:-}" ]; then
            /opt/terraform/extract-terraform.sh
        fi
        ;;
esac

exec /usr/local/bin/entrypoint-bin "$@"
