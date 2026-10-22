#!/usr/bin/env node
// Renovate postUpgradeTask: rewrites a matrix to the newest patch of the last
// WINDOW minor lines. Renovate's own bump only triggers the run; this sets the values.
import { readFileSync, writeFileSync } from "node:fs";

const WINDOW = 3;

const TARGETS = {
  vault: {
    image: "registry-1.docker.io/hashicorp/vault",
    files: [".github/workflows/e2e.yml", "test/e2e/run.sh"],
  },
};

async function registryGet(url, token) {
  const headers = token ? { Authorization: `Bearer ${token}` } : {};
  return fetch(url, { headers });
}

async function anonymousToken(challenge) {
  const params = Object.fromEntries(
    [...challenge.matchAll(/(\w+)="([^"]*)"/g)].map(([, k, v]) => [k, v]),
  );
  const url = new URL(params.realm);
  for (const key of ["service", "scope"]) {
    if (params[key]) url.searchParams.set(key, params[key]);
  }
  const res = await fetch(url);
  if (!res.ok) throw new Error(`token ${url}: HTTP ${res.status}`);
  const body = await res.json();
  return body.token ?? body.access_token;
}

async function listTags(image) {
  const [host, ...rest] = image.split("/");
  const base = `https://${host}`;
  let next = `${base}/v2/${rest.join("/")}/tags/list?n=1000`;
  let token;
  const tags = [];
  while (next) {
    let res = await registryGet(next, token);
    if (res.status === 401 && !token) {
      token = await anonymousToken(res.headers.get("www-authenticate") ?? "");
      res = await registryGet(next, token);
    }
    if (!res.ok) throw new Error(`${next}: HTTP ${res.status}`);
    tags.push(...((await res.json()).tags ?? []));
    const link = res.headers.get("link")?.match(/<([^>]+)>/)?.[1];
    next = link ? new URL(link, base).href : null;
  }
  return tags;
}

export function window(tags, size = WINDOW) {
  const newestPerMinor = new Map();
  for (const tag of tags) {
    const m = tag.match(/^(\d+)\.(\d+)\.(\d+)$/);
    if (!m) continue;
    const v = m.slice(1).map(Number);
    const key = `${v[0]}.${v[1]}`;
    const prev = newestPerMinor.get(key);
    if (!prev || v[2] > prev[2]) newestPerMinor.set(key, v);
  }
  return [...newestPerMinor.values()]
    .sort((a, b) => b[0] - a[0] || b[1] - a[1])
    .slice(0, size)
    .map((v) => v.join("."));
}

export function rewrite(text, name, versions) {
  const env = `${name.toUpperCase()}_VERSION`;
  return text
    .replace(
      new RegExp(`^(\\s*${name}: )\\[[^\\]\\n]*\\]`, "m"),
      `$1[${versions.map((v) => `'${v}'`).join(", ")}]`,
    )
    .replaceAll(new RegExp(`(\\$\\{${env}:-)[^}]+\\}`, "g"), `$1${versions[0]}}`)
    .replaceAll(new RegExp(`(${env} \\(default )[^)]+\\)`, "g"), `$1${versions[0]})`);
}

async function main() {
  const name = process.argv[2];
  const target = TARGETS[name];
  if (!target) throw new Error(`usage: version-matrix.mjs <${Object.keys(TARGETS).join("|")}>`);
  const versions = window(await listTags(target.image));
  if (versions.length < WINDOW) throw new Error(`${target.image}: only found ${versions}`);
  for (const file of target.files) {
    const before = readFileSync(file, "utf8");
    const after = rewrite(before, name, versions);
    if (after !== before) writeFileSync(file, after);
  }
  console.log(`${name}: ${versions.join(", ")}`);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main().catch((err) => {
    console.error(err.message);
    process.exit(1);
  });
}
