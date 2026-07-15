import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const assetsDir = path.resolve(root, "../../internal/dashboard/assets");

mkdirSync(assetsDir, { recursive: true });
writeFileSync(path.join(assetsDir, ".production"), "1\n", "utf8");
// Keep the assets dir trackable when empty between builds.
writeFileSync(path.join(assetsDir, ".gitkeep"), "", "utf8");
