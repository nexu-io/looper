#!/usr/bin/env bun

import { bootstrapLooperd } from "./bootstrap/index";
import { ConfigValidationError } from "./config/index";

try {
  await bootstrapLooperd({
    argv: Bun.argv.slice(2),
    env: process.env,
    waitForShutdown: true,
  });
} catch (error) {
  if (error instanceof ConfigValidationError) {
    console.error("looperd failed to start due to invalid configuration:");

    for (const issue of error.issues) {
      console.error(`- ${issue.path}: ${issue.message}`);
    }

    process.exit(1);
  }

  throw error;
}
