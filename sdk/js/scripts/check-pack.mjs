// Asks npm which files it would publish, and fails if the compiled client
// is missing or if sources and tests would be shipped. Nothing is uploaded.
import { execFileSync } from "node:child_process";

const out = execFileSync("npm", ["pack", "--dry-run", "--json"], {
  encoding: "utf8",
  stdio: ["ignore", "pipe", "ignore"],
  shell: process.platform === "win32",
});
const files = JSON.parse(out)[0].files.map((f) => f.path);

const required = ["dist/index.js", "dist/index.d.ts", "package.json", "README.md"];
const forbidden = ["src/", "test/", "scripts/", "node_modules/"];

const problems = [
  ...required.filter((p) => !files.includes(p)).map((p) => `missing ${p}`),
  ...files.filter((p) => forbidden.some((dir) => p.startsWith(dir))).map((p) => `should not contain ${p}`),
];
if (problems.length > 0) {
  for (const p of problems) console.error(`npm package: ${p}`);
  process.exit(1);
}
console.log(`npm package ok: ${files.length} files`);
