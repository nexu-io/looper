import { homedir } from "node:os";
import { join } from "node:path";

import {
  LOOPERD_BUILD_METADATA,
  LOOPERD_VERSION,
  type LooperdBuildMetadata,
} from "./generated/version";

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

export interface LooperdBuildInfo {
  version: string;
  metadata: LooperdBuildMetadata;
}

export const LOOPERD_BUILD_INFO: LooperdBuildInfo = {
  version: LOOPERD_VERSION,
  metadata: LOOPERD_BUILD_METADATA,
};

export { LOOPERD_BUILD_METADATA, LOOPERD_VERSION };
