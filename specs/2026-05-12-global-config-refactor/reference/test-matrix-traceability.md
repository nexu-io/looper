# Global config refactor test matrix traceability

Date: 2026-05-13

This list traces the Ralph test-matrix task items to concrete automated tests.

| Matrix item | Tests |
| --- | --- |
| legacy-only config | `TestLegacyAndCanonicalConfigSurfacesProduceEquivalentTargets`; `TestLegacyReviewerConfigStillValidatesAgainstCanonicalRules`; `TestLegacyProjectInstructionsNormalizeToCanonicalProjectRoleInstructions` |
| canonical-only config | `TestCanonicalDomainsRoundTripAcrossStructuredFormats`; `TestConfigEditCreatesCanonicalTemplateAtSelectedTOMLPath` |
| mixed config where canonical wins | `TestMixedSchemaConfigAcceptsDeterministicInputsWithCanonicalWinning`; `TestMixedSchemaEnvAndCLIOverridesStillBeatFileBackedValues` |
| JSON/YAML/TOML parity | `TestLoadFileLoadsSupportedConfigFormats`; `TestLoadFileMatchesFrozenParityFixtures`; `TestCanonicalDomainsRoundTripAcrossStructuredFormats` |
| explicit null/empty/omitted parity | `TestLoadFileUsesDefaultsWhenConfigFileIsTopLevelNull`; `TestReadConfigFileAcceptsTopLevelNull`; `TestLoadFileNullEmptyAndOmittedShapesProduceEquivalentDefaultsAcrossFormats` |
| unsupported config-suffix errors | `TestLoadFileRejectsUnsupportedConfigSuffixWithExactMessage` |
| `--config` / `LOOPER_CONFIG` / default discovery precedence | `TestLoadFileConfigPathSelectionPrefersCLIThenEnvThenOptions`; `TestLoadFileConfigPathSelectionPrefersCLIThenEnvThenDiscoveredDefault` |
| multiple-default ambiguity errors | `TestLoadFileRejectsMultipleDefaultConfigFiles` |
| no-default behavior | `TestLoadFileUsesDefaultsWhenConfigMissing`; `TestLoadFileUsesDefaultConfigPathWhenUnset`; `TestLoadFileMatchesFrozenParityFixtures/default-path-missing` |
| project override inheritance and precedence | `TestProjectRoleConfigOverridesGlobalRoleConfig`; `TestEnvOverrideReviewerEnableSelfReviewBeatsProjectConfig`; `TestProjectRoleInstructionsCanClearGlobalInstructions` |
| legacy project conflicts with canonical global/project config | `TestMixedSchemaConfigAcceptsDeterministicInputsWithCanonicalWinning/legacy project instructions lose to canonical project role instructions`; `TestMixedSchemaConfigAcceptsDeterministicInputsWithCanonicalWinning/legacy project reviewer discovery loses to canonical discovery`; `TestCanonicalProjectRoleInstructionsBeatLegacyProjectInstructionMap` |
| canonical and legacy env/CLI alias behavior | `TestLegacyAndCanonicalFixerEnvOverridesProduceEquivalentTargets`; `TestLegacyAndCanonicalReviewerEnvOverridesResolveIdenticallyAndBeatFileConfig`; `TestLegacyAndCanonicalReviewerCLIOverridesResolveIdenticallyAndBeatFileConfig`; `TestLoadFileReviewerLoopPrecedenceDefaultsFileEnvCLI`; `TestLoadFileReviewerReviewEventsPrecedenceDefaultsFileEnvCLI` |
| deprecation warnings | `TestDeprecatedAliasWarningsDeduplicateAndUseExactReplacementNames`; `TestLoadFileLegacyDefaultConfigJSONEmitsMigrationNote`; `TestConfigValidatePrintsLegacyDefaultConfigMigrationNote`; `TestEmitConfigLoadNoticesPrintsEachNoticeOncePerRuntime` |
| deep-merge and array-replacement semantics | `TestNormalizeLayersKeepDeepMergeForObjectsAndArrayReplacementForArrays`; `TestNormalizeLayersProduceEquivalentEffectiveConfigAcrossCanonicalLegacyAndMixedInputs`; `TestNormalizeAppliesOverridesWithoutDroppingDefaults` |
| config-writing/bootstrap/init behavior | `TestBootstrapDefaultPathReusesExistingLegacyDefaultConfig`; `TestBootstrapDefaultPathPrefersCanonicalTOMLWhenNoDefaultExists`; `TestEnsureBootstrapConfigPreservesExplicitYAMLFormat`; `TestBootstrapAddsProjectWithoutPersistingRuntimeOverrides`; `TestConfigEditCreatesCanonicalTemplateAtSelectedTOMLPath`; `TestConfigSetPreservesSelectedYAMLFormat` |
| unchanged default behavior for users who do not migrate | `TestDefaultConfigMatchesDaemonDefaults`; `TestRoleDefaultsMirrorCurrentDiscoveryPolicy`; `TestLoadFileUsesDefaultsWhenConfigMissing` |
