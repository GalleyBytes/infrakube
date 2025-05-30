#!/bin/bash
# Usage:
#   /bin/bash hack/bundler.sh v0.9.0
#
# To be executed from root of repo
set -o errexit
set -o nounset
set -o pipefail

ver=$1
name="${2:-}"

bundle_dir="deploy/bundles/$ver"
mkdir -p $bundle_dir

bundle="$bundle_dir/${name:-$ver}.yaml"
cat /dev/null > $bundle
printf -- '---\n# namespace\n' >>  $bundle
cat deploy/namespace.yaml >> $bundle
printf -- '\n\n---\n# serviceaccount\n' >>  $bundle
cat deploy/serviceaccount.yaml >> $bundle
printf -- '\n\n---\n# clusterrole\n' >>  $bundle
cat deploy/clusterrole.yaml >> $bundle
printf -- '\n\n---\n# clusterrolebinding\n' >>  $bundle
cat deploy/clusterrolebinding.yaml >> $bundle
printf -- '\n\n---\n# deployment\n' >>  $bundle
yq write deploy/deployment.yaml spec.template.spec.containers[0].image "ghcr.io/galleybytes/terraform-operator:$ver" >> $bundle
printf -- '\n\n---\n# crd\n' >>  $bundle
cat deploy/crds/infra3.galleybytes.com_tfs_crd.yaml >> $bundle

>&2 printf "Saved "
printf "$bundle\n"

read -r -p 'Do you want to push new bundle to origin master? ' choice
case "$choice" in
  n|N) exit 0;;
  *) echo '';;
esac
sed -i '' s,deploy/bundles/.*,$bundle, README.md
git add "$bundle" README.md
git commit -m "make bundle $ver"
git checkout -B master
# Never force this to ensure coherent + sequential history
git push origin master
