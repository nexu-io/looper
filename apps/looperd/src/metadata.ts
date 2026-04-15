import { homedir } from "node:os";
import { join } from "node:path";

import { LOOPERD_VERSION } from "./generated/version";

export const LOOPERD_BINARY_BASENAME = "looperd";
export const LOOPERD_INSTALL_DIR = join(homedir(), ".looper", "bin");
export const LOOPERD_SUPPORTED_TARGETS = [
  "darwin-arm64",
  "darwin-x64",
] as const;

export type LooperdSupportedTarget = (typeof LOOPERD_SUPPORTED_TARGETS)[number];

export function isLooperdSupportedTarget(
  value: string,
): value is LooperdSupportedTarget {
  return LOOPERD_SUPPORTED_TARGETS.includes(value as LooperdSupportedTarget);
}

export function getLooperdArtifactName(target: LooperdSupportedTarget): string {
  return `${LOOPERD_BINARY_BASENAME}-${target}`;
}

export function getCurrentLooperdTarget(): string {
  return `${process.platform}-${process.arch}`;
}

export { LOOPERD_VERSION };
