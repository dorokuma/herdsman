# Releasing Herdsman

Herdsman publishes two npm packages and one GitHub-distributed Herdr integration.

| Artifact | Distribution |
| --- | --- |
| `@dorokuma/herdsman` | Public npm package and `herdsman` CLI |
| `@dorokuma/herdsman-pi` | Public npm package installed by Pi |
| `packages/herdsman-herdr-plugin` | GitHub repository subdirectory installed by Herdr |

Do not run `npm publish` from `packages/herdsman-herdr-plugin`. Its private package manifest supports local validation only.

## Preconditions

Run releases from the repository root on `main`. Replace the version below with the version being released.

```bash
export VERSION=0.13.2
export TAG="v$VERSION"
export PATH="$HOME/.local/share/mise/installs/node/26.7.0/bin:$HOME/.local/share/mise/installs/pnpm/11.9.0:$PATH"

git fetch origin main
test "$(git branch --show-current)" = "main"
test -z "$(git status --porcelain)"
test "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)"
test "$(npm whoami)" = "dorokuma"
gh auth status
```

The npm account must have verified email and write 2FA. Never put an npm token or OTP in the repository, shell history, release notes, or chat.

`npm profile get` is deliberately absent above: the CI/automation token cannot read the account profile, so the command always fails with

```text
npm error code E403
npm error 403 403 Forbidden - GET https://registry.npmjs.org/-/npm/v1/user
```

Reading that failure as "not logged in" would abort a release that can publish perfectly well. Nothing here substitutes for reading the account profile, so the gate is the combination above plus the checks later in this document: `npm whoami` must return the publisher, both target versions must still be absent (`E404`, below), `gh auth status` must pass, and both tarballs must pack and install into isolated prefixes. Together those checks establish publish rights, version availability, GitHub authentication, and package contents — not account posture, which is handled on its own just below.

Account posture — verified email and write 2FA — is outside what the automated gate can cover: the token cannot read the account profile, so no command listed here can prove it. The write path has at least been exercised for real: the registry accepted both `npm publish` PUTs for 0.13.2, a real exercise of publish rights (the same token had already carried a successful two-package release, 0.12.1, on 2026-09-28). A rejected publish changes nothing in the registry, so fix the account and retry the same version. The account-level check surface is `https://www.npmjs.com/settings/dorokuma/profile`; a page-level check there **has not been recorded yet** and remains a one-time action to do — do it no later than the next release (a suggested time, not a gate) — whoever performs it records who, when, and what they saw in the same release note that carries the limitation below.

Skipping `npm profile get` is a permanent known limitation of the automation token, not a per-release deviation: record it **once** in a release note under `.agents/notes/`, with the `E403` output above as evidence, and do not re-record it on every release. The same note holds the account-level check above — not a completed confirmation but its state (not recorded yet) and, once performed, who did it, when, and what they saw — so the limitation and the confirmation live in a single record instead of two. That note is the one created by the `Record the release in .agents/notes/` step of `## Publish the tag and GitHub Release`.

Confirm the version does not exist:

```bash
npm view "@dorokuma/herdsman@$VERSION" version
npm view "@dorokuma/herdsman-pi@$VERSION" version
```

Both commands must return `E404`. Stop if either command prints a version.

## Update versions

Keep these files synchronized:

- `package.json`
- `packages/herdsman-pi/package.json`
- `packages/herdsman-herdr-plugin/package.json`
- `packages/herdsman-herdr-plugin/herdr-plugin.toml`

The following command updates all four:

```bash
node --input-type=module <<'NODE'
import { readFile, writeFile } from "node:fs/promises";

const version = process.env.VERSION;
if (!version) throw new Error("VERSION is required");

for (const path of [
  "package.json",
  "packages/herdsman-pi/package.json",
  "packages/herdsman-herdr-plugin/package.json",
]) {
  const manifest = JSON.parse(await readFile(path, "utf8"));
  manifest.version = version;
  await writeFile(path, `${JSON.stringify(manifest, null, 2)}\n`);
}

const tomlPath = "packages/herdsman-herdr-plugin/herdr-plugin.toml";
const toml = await readFile(tomlPath, "utf8");
const updated = toml.replace(/^version = "[^"]+"$/m, `version = "${version}"`);
if (updated === toml) throw new Error("Herdr plugin version was not updated");
await writeFile(tomlPath, updated);
NODE
```

Review the four-manifest diff before continuing.

Then replace the Herdr install tag in both READMEs so they point at the tag being created. The tag does not exist until this release creates it, so never write a version that has no tag:

- `README.md` (Herdr plugin section)
- `packages/herdsman-herdr-plugin/README.md`

```bash
grep -rn -- '--ref v' README.md packages/herdsman-herdr-plugin/README.md
```

Both lines must show `--ref $TAG`.

## Update the CHANGELOG

A released version section carries facts that hold **at the moment the release commit is written** — no tense judgement, and no registry outcome. Two accepted forms:

- the default, following the 0.12.1 precedent: no status line at all, only the "版本与引用同步" and "范围" lines;
- or a status line limited to facts already true when the commit is written, for example the scope ("本版本包含 … 的全部内容") plus the version and reference synchronization.

Inside a version section that has not been published yet, every line has to describe only what already holds at the moment the commit is written. Do not use tense words such as `已发布` / `released` there, and do not preset events that have not happened yet: the 0.13.2 wording "随发布提交同步至本版本创建的 `v0.13.2`" is the counter-example — the tag it names did not exist when that line was written, so the line asserted an outcome the commit itself could not show. The "版本与引用同步" line is bounded by the same moment; the 0.12.1 section is the precedent for the recommended form, a plain description of the current state that makes no claim about what the release will produce.

Registry-side facts only become true after publication: that npm `latest` advanced from A to B in both packages, that both packages are visible, that the pushed tag points at the release commit. Keep them out of the CHANGELOG release commit — state them in the GitHub Release body (see `## Publish the tag and GitHub Release`) and in the release note under `.agents/notes/` created when the release ends, or when the release aborts having touched the registry, a tag, or a Release (see `## Recover from a partial publication`).

Reserve **未发布** for version sections that have not been published yet. The reason is that the repository is committed and pushed before the artifacts exist in the registry: any line asserting where the release stands can contradict the registry in that window, and aborting or re-versioning a release then costs an extra corrective commit. `CHANGELOG.md` is part of the release commit (see the `git add` list below), so it must be final before that commit. In the release commit itself no release-status word may remain: a `未发布` marker left from an earlier bump is deleted there by default — or replaced with one of the fact lines above — because the release commit is final, and both the tag snapshot and the Release are taken from it. `未发布` therefore serves only a bump that has not shipped yet, and is cleared as soon as its section enters the release commit.

The 0.13.2 section — the `**已发布**` line at the top of `CHANGELOG.md` — predates this rule and is not rewritten retroactively; it is already sealed in the `v0.13.2` tag and its GitHub Release. The rule above applies from the next release onwards.

Once the four manifests, both READMEs, and the CHANGELOG release section are all updated, review the full version diff before continuing.

## Validate source and package contents

```bash
pnpm check
pnpm build
git diff --check
```

`pnpm check` includes root, Pi, and Herdr package checks. The root package checker rebuilds from a clean `dist` directory and rejects source, tests, plans, nested packages, assets, and stale `worker` paths.

Create the two public tarballs outside the repository:

```bash
export RELEASE_TMP="$(mktemp -d)"
npm pack --pack-destination "$RELEASE_TMP"
(
  cd packages/herdsman-pi
  npm pack --pack-destination "$RELEASE_TMP"
)

EXPECTED_TARBALLS="$(printf '%s\n' \
  "dorokuma-herdsman-$VERSION.tgz" \
  "dorokuma-herdsman-pi-$VERSION.tgz")"
ACTUAL_TARBALLS="$(find "$RELEASE_TMP" -maxdepth 1 -type f -name '*.tgz' \
  -exec basename {} \; | sort)"
test "$ACTUAL_TARBALLS" = "$EXPECTED_TARBALLS"
```

Install both tarballs in isolated prefixes:

```bash
npm install --global --prefix "$RELEASE_TMP/root-prefix" \
  "$RELEASE_TMP/dorokuma-herdsman-$VERSION.tgz"
"$RELEASE_TMP/root-prefix/bin/herdsman" help

npm install --prefix "$RELEASE_TMP/pi-prefix" --ignore-scripts \
  "$RELEASE_TMP/dorokuma-herdsman-pi-$VERSION.tgz"
test -f "$RELEASE_TMP/pi-prefix/node_modules/@dorokuma/herdsman-pi/src/index.ts"
test -f "$RELEASE_TMP/pi-prefix/node_modules/@dorokuma/herdsman-pi/LICENSE"
test ! -f "$RELEASE_TMP/pi-prefix/node_modules/@dorokuma/herdsman-pi/tsconfig.json"
```

Do not continue unless both installations pass.

## Commit and create a local tag

```bash
git add \
  package.json \
  packages/herdsman-pi/package.json \
  packages/herdsman-herdr-plugin/package.json \
  packages/herdsman-herdr-plugin/herdr-plugin.toml \
  README.md \
  packages/herdsman-herdr-plugin/README.md \
  CHANGELOG.md
git commit -m "chore(release): $VERSION"
test -z "$(git status --porcelain)"
git push origin main
test "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)"
git tag -a "$TAG" -m "$TAG"
test "$(git rev-list -n 1 "$TAG")" = "$(git rev-parse HEAD)"
```

The staged set for the release commit is exactly four manifests plus both READMEs plus `CHANGELOG.md`; anything left unstaged fails the `git status --porcelain` check above and aborts the release.

The tag must be created **after** the release commit — the commit that carries the README install tag and the CHANGELOG release section — has been pushed to `main`, which is why `git tag -a` follows `git push origin main` and the `HEAD` = `origin/main` check above. Verify the tag snapshot before publishing anything:

```bash
git rev-list -n 1 "$TAG"
git rev-parse HEAD
git rev-parse origin/main
git show "$TAG:README.md" | grep -- '--ref v'
git show "$TAG:packages/herdsman-herdr-plugin/README.md" | grep -- '--ref v'
git show "$TAG:package.json" | grep -F -- "\"version\": \"$VERSION\""
git show "$TAG:CHANGELOG.md" | grep -F -- "## $VERSION"
```

`git rev-list -n 1 "$TAG"` must equal both `HEAD` and `origin/main`; both README lines must show `--ref $TAG`; the tagged `package.json` must carry `$VERSION`; and the tagged `CHANGELOG.md` must contain this version's heading together with its fact lines (at minimum the "版本与引用同步" and "范围" lines). Creating the tag earlier seals whatever the commit contained at that moment into the tag snapshot: a README still pointing at the previous tag, a manifest still at the previous version, or a CHANGELOG without this version's heading would stay that way forever, because tags must never be moved (see the recovery rules below).

Keep the tag local until both npm packages have been published and verified.

## Publish to npm

Publish from the repository root with the locally configured npm
account (`npm whoami` must return `dorokuma`). The account uses a
CI/automation token with write access, so no interactive second factor
is requested and coding agents can run the publication directly.

Publish the root package from the repository root:

```bash
npm publish --access public
```

If the command exits successfully but the registry returns E404 during
subsequent verification, the CDN is still propagating: wait and retry
`npm view` instead of republishing. Republishing a staged version
returns E409; a successful publication that is not yet visible in
`npm view` resolves within a minute.

Verify the exact version:

```bash
npm view "@dorokuma/herdsman@$VERSION" \
  name version dist-tags.latest repository bin --json
```

Then publish the Pi package in a separate command:

```bash
(
  cd packages/herdsman-pi
  npm publish --access public
)
```

After publication, verify the exact version:

```bash
npm view "@dorokuma/herdsman-pi@$VERSION" \
  name version dist-tags.latest repository peerDependencies --json
```

After a timeout or network error, query the exact version before retrying. Do not retry when `npm view` shows that version.

## Verify registry installation

Use a new directory so this check cannot read the local tarballs:

```bash
export REGISTRY_TMP="$(mktemp -d)"
npm install --global --prefix "$REGISTRY_TMP/root-prefix" \
  "@dorokuma/herdsman@$VERSION"
"$REGISTRY_TMP/root-prefix/bin/herdsman" help

npm install --prefix "$REGISTRY_TMP/pi-prefix" --ignore-scripts \
  "@dorokuma/herdsman-pi@$VERSION"
test -f "$REGISTRY_TMP/pi-prefix/node_modules/@dorokuma/herdsman-pi/src/index.ts"
```

## Publish the tag and GitHub Release

Write release notes to `/tmp/herdsman-$VERSION-release-notes.md`. Include both npm install commands, package-content changes, validation, and the fact that Herdr still installs its plugin from GitHub.

The Release body also carries the registry-side facts, which are true and checkable by this point: `dist-tags.latest` advanced from the previous version to `$VERSION` in both packages, both `@dorokuma/herdsman@$VERSION` and `@dorokuma/herdsman-pi@$VERSION` are visible in the registry, and the pushed tag `$TAG` points at the release commit.

```bash
git push origin "$TAG"
gh release create "$TAG" \
  --verify-tag \
  --title "$TAG" \
  --notes-file "/tmp/herdsman-$VERSION-release-notes.md" \
  --latest
```

Verify every external artifact:

```bash
npm view "@dorokuma/herdsman@$VERSION" version
npm view "@dorokuma/herdsman-pi@$VERSION" version
gh release view "$TAG" --json tagName,name,isDraft,isPrerelease,url,publishedAt
gh api repos/dorokuma/herdsman/releases/latest --jq .tag_name
git ls-remote --tags origin "refs/tags/$TAG" "refs/tags/$TAG^{}"
test -z "$(git status --porcelain)"
```

**Record the release in `.agents/notes/`.** The release is not finished until its release note exists under `.agents/notes/`. Name it after the directory's convention — `YYYYMMDD-slug.md`, release date plus a short slug, as in `.agents/notes/README.md` — and reuse the front matter of the neighbouring notes (`status`, `supersedes`, `superseded_by`, `模块`; see `.agents/notes/_template.md`).

The note records at least:

- the registry-side facts: `dist-tags.latest` moved from the previous version to `$VERSION` in both `@dorokuma/herdsman` and `@dorokuma/herdsman-pi`; both `@dorokuma/herdsman@$VERSION` and `@dorokuma/herdsman-pi@$VERSION` are visible in the registry; and `git rev-list -n 1 "$TAG"` peels the pushed tag to the release commit;
- the `sha256sum` of both tarballs packed in `$RELEASE_TMP`, plus the conclusion that each matches the published `dist.integrity` (`npm view "<pkg>@$VERSION" dist.integrity`). `sha256sum` and `dist.integrity` cannot be compared directly — the integrity hash is base64 `sha512` with a `sha512-` prefix — so recompute the whole string, prefix included, in one copy-pasteable command: `printf 'sha512-%s' "$(openssl dgst -sha512 -binary <tgz> | openssl base64 -A)"`, then compare its output with `dist.integrity`; the two must agree;
- the Release URL, for example from `gh release view "$TAG" --json url --jq .url`.

An aborted release writes this note too whenever the abort touched the registry, a tag, or a Release — a clean abort that touched none of those has no account to keep and may skip it (see `## Recover from a partial publication`).

## Recover from a partial publication

Two npm publishes cannot be atomic. Use these rules when the root version exists but the Pi package needs a content change. This flow covers that case only: once **both** packages are published, a later discovery that the Pi package needs a content change is outside it — stop there and ask the user instead of assembling a sequence from this section.

1. Confirm the Pi version is absent with `npm view`.
2. Roll `latest` back to the last complete release, so `latest` does not resolve to a half-published version while the version level is still being decided. `<lastGood>` is authoritative as the Pi package's `dist-tags.latest` **before** the rollback: step 1 has already shown that this release never moved the Pi package, so that value is still the last complete release. Read and keep both values before touching anything. Expected values when this flow is entered: the root package's previous `dist-tags.latest` should be this round's orphaned `$VERSION`, and the Pi package's should be the last complete release in the step 4 sense — the last version both packages published successfully. If either read disagrees, stop and ask the user instead of rolling back onto a wrong baseline; comparing the two values only with each other cannot catch this, because the check after the rollback compares pre-rollback and post-rollback values from the same source.

```bash
npm view @dorokuma/herdsman dist-tags.latest
npm view @dorokuma/herdsman-pi dist-tags.latest
```

Then point both packages at `<lastGood>`, using the same credentials and environment as the publication (repository root, the `PATH` from `## Preconditions`):

```bash
npm dist-tag add "@dorokuma/herdsman@<lastGood>" latest
npm dist-tag add "@dorokuma/herdsman-pi@<lastGood>" latest
```

Verify and keep the result immediately afterwards:

```bash
npm view @dorokuma/herdsman dist-tags.latest
npm view @dorokuma/herdsman-pi dist-tags.latest
```

Both values must equal `<lastGood>`. Stop if they do not: two packages disagreeing on `latest` is exactly the half-published state this step exists to clear.

A failed rollback leaves the registry as it was — `latest` keeps its previous value and no version, artifact, or tag changes — so fix the command and retry. Neither the failure nor the rollback blocks the replacement-version mainline below: publishing the replacement moves `latest` forward again on its own. Users who installed the incomplete root version inside that window are not repaired retroactively; they are covered by the replacement version once it ships.

3. Delete only the local, unpushed tag: `git tag -d "$TAG"`.
4. Export the next unused version. The version level is a user decision: the agent asks and must not settle it alone; this document only supplies the calculated default. Criterion: measure from the **last complete release** — the last version both packages published successfully — not from the orphaned root version; if this partial publication contains a breaking change (something users can feel, such as a removed public API, CLI command, or behaviour), use the next unused minor, otherwise the next unused patch. For example `export VERSION=0.3.2 TAG=v0.3.2`.
5. Update all four version files, replace the Herdr tag in `README.md` and `packages/herdsman-herdr-plugin/README.md`, and correct the CHANGELOG section for the replacement version (a released section carries only facts, so a replacement means editing that section, not adding a second one).
6. Rebuild and reinstall both tarballs.
7. Commit the replacement version and documentation, confirm the tree is clean, push `main`, and verify `HEAD` equals `origin/main`.
8. Create a new local tag from the pushed replacement commit.
9. Publish and verify both packages at the replacement version.
10. Push only the replacement tag and create only its GitHub Release.
11. After the complete replacement exists, the orphaned root version becomes the deprecation candidate below; whether to run that command is a user decision, and the replacement release's note records the voided orphan root version together with that decision:

```bash
npm deprecate @dorokuma/herdsman@0.3.1 \
  "Incomplete paired release; use 0.3.2"
```

Do not move a remote tag, overwrite a GitHub Release, reuse an npm version, or unpublish a package to repair a release.

Two different pointers are both called a tag in this document. A **git tag** (`vX.Y.Z`) is a fixed pointer into history: once pushed it is never moved, deleted, or recreated, which is what the sentence above forbids. An **npm dist-tag** (`latest`) is a movable pointer in the registry: step 2 rolls it back on purpose and the replacement publication moves it forward again. The prohibition covers the git tag, not `latest`.

**Long-term abort.** Whether to abort long-term is a user decision, in the same sense as the version level in step 4: the agent asks and must not settle it alone. If the decision is not to change the version and not to publish a replacement, the release commit already pushed to `main` still carries wording that no longer matches reality. Fix that wording with one explicit corrective commit on `main` — never by rewriting pushed history. If the tag was already pushed, leave it exactly where it is: remote tags are never moved, deleted, or recreated, and the corrective commit does not touch it. An already-created GitHub Release gets the same treatment: leave it in place and do not edit its body. That ordering is close to unreachable in the standard flow — the Release is created only after the tag is pushed and both packages are verified — so treat it as a defensive branch, not a scenario to plan around.

Whether to deprecate the orphaned root version in this branch is likewise a user decision, and this document prescribes no steps for it.

The corrective commit has to cover every artifact that would otherwise point at a release that will not exist:

- the Herdr install tag in `README.md` and `packages/herdsman-herdr-plugin/README.md`: both carry `--ref $TAG` and would point at a tag that is never created;
- the `CHANGELOG.md` section written for this version — its heading, its status line if it has one, and its "版本与引用同步" / "范围" lines;
- the four manifest version fields: keeping `$VERSION` there or rolling them back to the last released version is a user decision, in the same sense as the version level in step 4 — the agent proposes and must not settle it alone; state the outcome in the corrective commit, because a manifest version no artifact carries is what the next release has to reconcile. Until that decision has been made, keep `$VERSION` and say so in the corrective commit;
- where the next release continues from: the last complete release remains the baseline, and the version number for this abandoned work is once again a user decision under the same rule as step 4.

The aborted release gets the release note described in `## Publish the tag and GitHub Release`, with a different subject: where a successful release records what shipped, an abort records the abort itself and the registry state at the moment of the decision — the `latest` values before and after the rollback, the last complete release, and the incomplete version being written off — plus the corrective commit that carried the wording change.
