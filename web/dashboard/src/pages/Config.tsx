import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { RefreshCw, Settings } from "lucide-react";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { ModelCombobox } from "@/components/ModelCombobox";
import { PanelError } from "@/components/PanelError";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  ApiError,
  fetchAgentModels,
  fetchConfig,
  patchConfig,
  type ConfigData,
  type ConfigFieldMetadata,
  type PatchConfigBody,
} from "@/lib/api";
import {
  AGENT_VENDOR_OPTIONS,
  agentModelScope,
  agentProfilePath,
  buildConfigPatch,
  CODING_ROLES,
  CONFIG_GROUPS,
  configFieldErrors,
  configFieldKind,
  configFieldLabel,
  configFieldPaths,
  configSelectOptions,
  draftFromValue,
  draftStagesConfigChange,
  effectiveAgentVendor,
  ESSENTIAL_PATHS,
  getConfigValue,
  highImpactChanges,
  isAgentProfileLeafPath,
  isAgentProfileWholePath,
  isCuratedAgentIdentityPath,
  isEssentialConfigPath,
  isRoleAgentLeafPath,
  isValidAgentProfileId,
  profileLeafUnsetWouldEmpty,
  ROLE_AGENT_FIELDS,
  roleAgentPath,
  type CodingRole,
  type ConfigDraft,
  type ConfigFieldKind,
  type ConfigGroup,
  type HighImpactChange,
} from "@/lib/configForm";
import { formatTs } from "@/lib/format";
import { useToast } from "@/lib/toast";

type ErrorMap = Record<string, string>;

const controlClass =
  "w-full rounded border border-[var(--border)] bg-[var(--bg)] px-2 py-1 text-[12px] text-[var(--text)] disabled:cursor-not-allowed disabled:opacity-60";

function metaIsEditable(meta: ConfigFieldMetadata | undefined): boolean {
  if (!meta?.editable) return false;
  return meta.source !== "env" && meta.source !== "cli";
}

const hotDefaultMeta: ConfigFieldMetadata = {
  source: "default",
  editable: true,
  applyMode: "hot",
};

/**
 * Prefer leaf metadata. Curated role-agent and profile entry/leaves fall back
 * to a hot default — never inherit the agent.profiles map-container metadata,
 * which the daemon marks non-editable/restart-bound while still accepting
 * profile leaf patches.
 */
function resolveFieldMeta(
  data: ConfigData,
  path: string,
): ConfigFieldMetadata | undefined {
  const direct = data.metadata.fields[path];
  if (direct) return direct;
  if (isRoleAgentLeafPath(path)) {
    return hotDefaultMeta;
  }
  if (isAgentProfileLeafPath(path) || isAgentProfileWholePath(path)) {
    return hotDefaultMeta;
  }
  return undefined;
}

/**
 * Profile add/remove is gated on entry/leaf editability, not the map container.
 * Real daemon metadata exposes agent.profiles as non-editable/restart-bound
 * while agent.profiles.<id>.vendor|model remain hot-editable.
 */
function agentProfilesEditableByAuthority(data: ConfigData): boolean {
  const fields = data.metadata.fields ?? {};
  let sawProfileEntryOrLeaf = false;
  for (const [path, meta] of Object.entries(fields)) {
    if (path === "agent.profiles" || !path.startsWith("agent.profiles.")) {
      continue;
    }
    sawProfileEntryOrLeaf = true;
    if (metaIsEditable(meta)) return true;
  }
  // No published entry/leaf metadata yet (empty map or only container listed):
  // allow via the same hot fallback used for missing leaf meta.
  if (!sawProfileEntryOrLeaf) return true;
  return false;
}

function sourceIsConfigFile(source: string | undefined): boolean {
  return source === "config-file" || source === "file";
}

function SourceBadge({ meta }: { meta?: ConfigFieldMetadata }) {
  const source = meta?.source ?? "unknown";
  const applyMode = meta?.applyMode ?? "unknown";
  return (
    <span className="flex shrink-0 items-center gap-1 mono text-[10px] text-[var(--text-muted)]">
      <span className="rounded border border-[var(--border)] px-1 py-px">
        {source}
      </span>
      {applyMode !== "hot" ? (
        <span className="rounded border border-[var(--warn)] px-1 py-px text-[var(--warn)]">
          {applyMode}
        </span>
      ) : null}
    </span>
  );
}

function FieldFrame({
  path,
  meta,
  error,
  dirty,
  unset,
  publishedValue,
  disabled,
  onUnset,
  children,
}: {
  path: string;
  meta?: ConfigFieldMetadata;
  error?: string;
  dirty: boolean;
  unset: boolean;
  publishedValue: unknown;
  disabled: boolean;
  onUnset: () => void;
  children: ReactNode;
}) {
  const editable = metaIsEditable(meta);
  return (
    <div
      className={`grid gap-1 border-b border-[var(--border)] py-2 last:border-b-0 sm:grid-cols-[minmax(180px,0.9fr)_minmax(220px,1.1fr)] sm:gap-3 ${
        dirty || unset ? "bg-[color-mix(in_srgb,var(--accent)_5%,transparent)]" : ""
      }`}
      data-config-path={path}
    >
      <div className="min-w-0">
        <div className="flex items-start justify-between gap-2">
          <label
            htmlFor={`config-${path}`}
            className="min-w-0 text-[12px] font-medium"
          >
            {configFieldLabel(path)}
          </label>
          <SourceBadge meta={meta} />
        </div>
        <code className="block break-all text-[10px] text-[var(--text-muted)]">
          {path}
        </code>
        {!editable ? (
          <p className="m-0 mt-0.5 text-[10px] text-[var(--text-muted)]">
            {meta?.source === "env" || meta?.source === "cli"
              ? `Read-only: ${meta.source.toUpperCase()} is the active authority.`
              : "Read-only in the dashboard."}
          </p>
        ) : null}
      </div>
      <div className="min-w-0">
        <div className="flex items-start gap-1.5">
          <div className={`min-w-0 flex-1 ${unset ? "opacity-50" : ""}`}>
            {children}
          </div>
          {editable && (sourceIsConfigFile(meta?.source) || unset) ? (
            <Button
              variant="ghost"
              size="sm"
              className="shrink-0"
              disabled={disabled}
              onClick={onUnset}
              title={
                unset
                  ? "Keep the current file value"
                  : "Remove this value from the config file"
              }
            >
              {unset ? "Undo" : "Unset"}
            </Button>
          ) : null}
        </div>
        {unset ? (
          <p className="m-0 mt-1 text-[10px] text-[var(--warn)]">
            Unsaved: will remove the file value on Save (next authority wins).
          </p>
        ) : null}
        {dirty || unset ? (
          <p className="m-0 mt-1 text-[10px] text-[var(--warn)]">
            Unsaved draft
            {unset ? null : (
              <>
                {" "}
                (was <code>{formatConfigValue(publishedValue)}</code>)
              </>
            )}
            . Click <strong>Save changes</strong> to apply.
          </p>
        ) : null}
        {error ? (
          <p className="m-0 mt-1 text-[11px] text-[var(--danger)]" role="alert">
            {error}
          </p>
        ) : null}
      </div>
    </div>
  );
}

function formatConfigValue(value: unknown): string {
  if (value === undefined) return "not configured";
  if (typeof value === "string") return value || "(empty string)";
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

function ConfigControl({
  path,
  kind,
  value,
  options,
  disabled,
  unset,
  onChange,
}: {
  path: string;
  kind: ConfigFieldKind;
  value: ConfigDraft;
  options?: string[];
  disabled: boolean;
  unset: boolean;
  onChange: (value: ConfigDraft) => void;
}) {
  const controlDisabled = disabled || unset;
  if (kind === "boolean") {
    return (
      <label className="inline-flex min-h-7 items-center gap-2 text-[12px]">
        <input
          id={`config-${path}`}
          aria-label={path}
          type="checkbox"
          checked={value === true}
          disabled={controlDisabled}
          onChange={(event) => onChange(event.currentTarget.checked)}
        />
        <span>{value === true ? "Enabled" : "Disabled"}</span>
      </label>
    );
  }

  if (options) {
    return (
      <select
        id={`config-${path}`}
        aria-label={path}
        className={controlClass}
        value={String(value)}
        disabled={controlDisabled}
        onChange={(event) => onChange(event.currentTarget.value)}
      >
        {String(value) === "" ? (
          <option value="" disabled>
            Not configured
          </option>
        ) : null}
        {options.map((option) => (
          <option key={option} value={option}>
            {option}
          </option>
        ))}
      </select>
    );
  }

  const multiline = kind === "array" || path.endsWith(".instructions");
  if (multiline) {
    return (
      <textarea
        id={`config-${path}`}
        aria-label={path}
        className={`${controlClass} min-h-16 resize-y mono`}
        value={String(value)}
        disabled={controlDisabled}
        placeholder={kind === "array" ? "One value per line" : undefined}
        onChange={(event) => onChange(event.currentTarget.value)}
      />
    );
  }

  return (
    <input
      id={`config-${path}`}
      aria-label={path}
      className={`${controlClass} ${kind === "number" ? "mono" : ""}`}
      type={kind === "number" ? "number" : "text"}
      step={kind === "number" ? 1 : undefined}
      value={String(value)}
      disabled={controlDisabled}
      onChange={(event) => onChange(event.currentTarget.value)}
    />
  );
}

function NewProfileModelSuggestions({ vendor }: { vendor: string }) {
  const [entries, setEntries] = useState<
    Array<{ id: string; label: string }>
  >([]);
  useEffect(() => {
    // Drop the previous vendor's suggestions immediately so the datalist
    // does not offer stale ids while the next probe is in flight or fails.
    setEntries([]);
    const controller = new AbortController();
    void (async () => {
      try {
        const data = await fetchAgentModels(vendor, {
          signal: controller.signal,
        });
        if (!controller.signal.aborted) setEntries(data.models);
      } catch {
        // Advisory only — silent. Do not restore previous vendor's entries.
      }
    })();
    return () => controller.abort();
  }, [vendor]);
  return (
    <datalist id="new-profile-model-suggestions">
      {entries.map((entry) => (
        <option key={entry.id} value={entry.id}>
          {entry.label && entry.label !== entry.id ? entry.label : ""}
        </option>
      ))}
    </datalist>
  );
}

function AgentProfiles({
  data,
  drafts,
  unsetPaths,
  errors,
  onDraft,
  onToggleUnset,
  onRemoveProfile,
  onUndoRemoveProfile,
  disabled,
}: {
  data: ConfigData;
  drafts: Record<string, ConfigDraft>;
  unsetPaths: Set<string>;
  errors: ErrorMap;
  onDraft: (path: string, value: ConfigDraft) => void;
  onToggleUnset: (path: string) => void;
  onRemoveProfile: (id: string, existed: boolean) => void;
  onUndoRemoveProfile: (id: string) => void;
  disabled: boolean;
}) {
  const [newId, setNewId] = useState("");
  const [newVendor, setNewVendor] = useState("");
  const [newModel, setNewModel] = useState("");
  const [localError, setLocalError] = useState<string | null>(null);

  // Section badge may still show map-container source; edit authority uses leaves.
  const profilesMeta =
    data.metadata.fields["agent.profiles"] ?? hotDefaultMeta;
  const editableByAuthority = agentProfilesEditableByAuthority(data);
  const canEdit = editableByAuthority && !disabled;

  const publishedIds = Object.keys(data.agent?.profiles ?? {}).sort();
  const stagedIds = new Set<string>();
  for (const path of Object.keys(drafts)) {
    const match = /^agent\.profiles\.([A-Za-z0-9_-]+)\.(vendor|model)$/.exec(
      path,
    );
    if (match) stagedIds.add(match[1]);
  }
  for (const path of unsetPaths) {
    const whole = /^agent\.profiles\.([A-Za-z0-9_-]+)$/.exec(path);
    if (whole) stagedIds.add(whole[1]);
    const leaf = /^agent\.profiles\.([A-Za-z0-9_-]+)\./.exec(path);
    if (leaf) stagedIds.add(leaf[1]);
  }
  const ids = [...new Set([...publishedIds, ...stagedIds])].sort();

  const stageNew = () => {
    const id = newId.trim();
    if (!isValidAgentProfileId(id)) {
      setLocalError("Profile id must match [A-Za-z0-9_-]+.");
      return;
    }
    if (ids.includes(id) && !unsetPaths.has(`agent.profiles.${id}`)) {
      setLocalError(`Profile "${id}" already exists.`);
      return;
    }
    const vendor = newVendor.trim();
    const model = newModel.trim();
    if (!vendor && !model) {
      setLocalError("Set at least vendor or model for the profile.");
      return;
    }
    // Clear whole-profile unset if re-adding after remove. Then stage only the
    // form leaves and unset any omitted published leaf so remove+recreate does
    // not silently keep the previous vendor/model.
    const reAddingAfterRemove = unsetPaths.has(`agent.profiles.${id}`);
    if (reAddingAfterRemove) {
      onUndoRemoveProfile(id);
    }
    const vendorPath = agentProfilePath(id, "vendor");
    const modelPath = agentProfilePath(id, "model");
    if (vendor) onDraft(vendorPath, vendor);
    if (model) onDraft(modelPath, model);
    if (reAddingAfterRemove) {
      const published = data.agent?.profiles?.[id];
      if (!vendor && published?.vendor != null && String(published.vendor) !== "") {
        onToggleUnset(vendorPath);
      }
      // Empty model ("") is a meaningful binding (suppresses inherited/params
      // models), so omit-on-recreate must unset it too — not only non-empty.
      if (!model && published?.model != null) {
        onToggleUnset(modelPath);
      }
    }
    setNewId("");
    setNewVendor("");
    setNewModel("");
    setLocalError(null);
  };

  return (
    <div
      className="mt-2 border-t border-[var(--border)] pt-2"
      data-testid="agent-profiles"
    >
      <div className="flex items-center justify-between gap-2">
        <div>
          <h3 className="m-0 text-[12px] font-medium">Agent profiles</h3>
          <p className="m-0 text-[10px] text-[var(--text-muted)]">
            Named vendor/model presets referenced by coding-role agent bindings.
            Params are not editable here.
          </p>
        </div>
        <SourceBadge meta={profilesMeta} />
      </div>

      <div className="mt-2 flex flex-col gap-2">
        {ids.length === 0 ? (
          <span className="text-[11px] text-[var(--text-muted)]">
            No agent profiles configured.
          </span>
        ) : null}
        {ids.map((id) => {
          const wholePath = `agent.profiles.${id}`;
          const vendorPath = agentProfilePath(id, "vendor");
          const modelPath = agentProfilePath(id, "model");
          const pendingRemoval = unsetPaths.has(wholePath);
          const exists = publishedIds.includes(id);
          const vendorMeta = resolveFieldMeta(data, vendorPath);
          const modelMeta = resolveFieldMeta(data, modelPath);
          const vendorEditable = metaIsEditable(vendorMeta) && canEdit;
          const modelEditable = metaIsEditable(modelMeta) && canEdit;
          const published = data.agent?.profiles?.[id];
          const vendorUnset = unsetPaths.has(vendorPath);
          const modelUnset = unsetPaths.has(modelPath);
          const vendorValue =
            vendorUnset || pendingRemoval
              ? ""
              : Object.hasOwn(drafts, vendorPath)
                ? String(drafts[vendorPath] ?? "")
                : published?.vendor == null
                  ? ""
                  : String(published.vendor);
          // null = persisted absence (Inherit); "" = explicit vendor-default
          // suppress. Do not collapse absence to "" — that mis-highlights
          // Vendor default and ArrowDown+Enter would stage set model:"".
          const modelValue: string | null =
            modelUnset || pendingRemoval
              ? null
              : Object.hasOwn(drafts, modelPath)
                ? String(drafts[modelPath] ?? "")
                : published?.model == null
                  ? null
                  : String(published.model);
          const canUnsetVendor =
            vendorEditable &&
            !pendingRemoval &&
            (sourceIsConfigFile(vendorMeta?.source) ||
              vendorUnset ||
              published?.vendor != null);
          const canUnsetModel =
            modelEditable &&
            !pendingRemoval &&
            (sourceIsConfigFile(modelMeta?.source) ||
              modelUnset ||
              published?.model != null);

          // Last remaining identity leaf (or both) must remove the profile —
          // leaf-only unsets leave agent.profiles.<id>={} which the daemon rejects.
          const toggleProfileLeafUnset = (field: "vendor" | "model") => {
            const path = agentProfilePath(id, field);
            if (unsetPaths.has(path)) {
              onToggleUnset(path);
              return;
            }
            if (
              exists &&
              profileLeafUnsetWouldEmpty(data, drafts, unsetPaths, id, field)
            ) {
              onRemoveProfile(id, true);
              return;
            }
            onToggleUnset(path);
          };

          return (
            <div
              key={id}
              className={`rounded border border-[var(--border)] px-2 py-1.5 ${
                pendingRemoval ? "opacity-60" : ""
              }`}
              data-config-path={wholePath}
            >
              <div className="flex flex-wrap items-center justify-between gap-1.5">
                <code
                  className={`text-[11px] ${pendingRemoval ? "line-through" : ""}`}
                >
                  {id}
                </code>
                <div className="flex items-center gap-1">
                  {pendingRemoval ? (
                    <button
                      type="button"
                      className="border-0 bg-transparent p-0 text-[10px] text-[var(--accent)]"
                      disabled={disabled}
                      onClick={() => onUndoRemoveProfile(id)}
                    >
                      undo remove
                    </button>
                  ) : (
                    <button
                      type="button"
                      aria-label={`Remove profile ${id}`}
                      className="border-0 bg-transparent p-0 text-[12px] text-[var(--danger)] disabled:opacity-40"
                      disabled={!canEdit}
                      onClick={() => onRemoveProfile(id, exists)}
                    >
                      ×
                    </button>
                  )}
                </div>
              </div>
              <div className="mt-1.5 grid gap-1.5 sm:grid-cols-2">
                <div className="min-w-0">
                  <div className="flex items-start gap-1.5">
                    <label className="min-w-0 flex-1 text-[11px]">
                      <span className="text-[var(--text-muted)]">Vendor</span>
                      <select
                        aria-label={vendorPath}
                        className={`${controlClass} mt-0.5 ${vendorUnset ? "opacity-50" : ""}`}
                        value={vendorValue}
                        disabled={
                          !vendorEditable || pendingRemoval || vendorUnset
                        }
                        onChange={(event) =>
                          onDraft(vendorPath, event.currentTarget.value)
                        }
                      >
                        {vendorValue === "" ? (
                          <option value="" disabled>
                            Not configured
                          </option>
                        ) : null}
                        {AGENT_VENDOR_OPTIONS.map((option) => (
                          <option key={option} value={option}>
                            {option}
                          </option>
                        ))}
                      </select>
                    </label>
                    {canUnsetVendor ? (
                      <Button
                        variant="ghost"
                        size="sm"
                        className="mt-4 shrink-0"
                        disabled={disabled}
                        aria-label={
                          vendorUnset
                            ? `Undo unset ${vendorPath}`
                            : `Unset ${vendorPath}`
                        }
                        title={
                          vendorUnset
                            ? "Keep the current file value"
                            : exists &&
                                profileLeafUnsetWouldEmpty(
                                  data,
                                  drafts,
                                  unsetPaths,
                                  id,
                                  "vendor",
                                )
                              ? "Remove profile (last identity leaf)"
                              : "Remove this value from the config file (inherit)"
                        }
                        onClick={() => toggleProfileLeafUnset("vendor")}
                      >
                        {vendorUnset ? "Undo" : "Unset"}
                      </Button>
                    ) : null}
                  </div>
                  {vendorUnset ? (
                    <p className="m-0 mt-1 text-[10px] text-[var(--warn)]">
                      Pending: remove profile vendor (inherit global).
                    </p>
                  ) : null}
                </div>
                <div className="min-w-0">
                  <div className="flex items-start gap-1.5">
                    <div className="min-w-0 flex-1 text-[11px]">
                      <span className="text-[var(--text-muted)]">Model</span>
                      <div className="mt-0.5">
                        <ModelCombobox
                          ariaLabel={modelPath}
                          vendor={effectiveAgentVendor(data, drafts, unsetPaths, {
                            kind: "profile",
                            id,
                          })}
                          value={modelValue}
                          unset={modelUnset}
                          disabled={!modelEditable || pendingRemoval}
                          allowInherit
                          placeholder="Model id (empty = vendor default; Inherit = previous layer)"
                          onCommitValue={(next) => {
                            // Empty draft stages model:"" (vendor default suppress).
                            // Use Unset for inheritance. Do not auto-unset on clear.
                            onDraft(modelPath, next);
                          }}
                          onInherit={() => toggleProfileLeafUnset("model")}
                        />
                      </div>
                    </div>
                    {canUnsetModel ? (
                      <Button
                        variant="ghost"
                        size="sm"
                        className="mt-4 shrink-0"
                        disabled={disabled}
                        aria-label={
                          modelUnset
                            ? `Undo unset ${modelPath}`
                            : `Unset ${modelPath}`
                        }
                        title={
                          modelUnset
                            ? "Keep the current file value"
                            : exists &&
                                profileLeafUnsetWouldEmpty(
                                  data,
                                  drafts,
                                  unsetPaths,
                                  id,
                                  "model",
                                )
                              ? "Remove profile (last identity leaf)"
                              : "Remove this value from the config file (inherit)"
                        }
                        onClick={() => toggleProfileLeafUnset("model")}
                      >
                        {modelUnset ? "Undo" : "Unset"}
                      </Button>
                    ) : null}
                  </div>
                  {modelUnset ? (
                    <p className="m-0 mt-1 text-[10px] text-[var(--warn)]">
                      Pending: remove profile model (inherit previous layer).
                    </p>
                  ) : null}
                </div>
              </div>
              {(errors[vendorPath] || errors[modelPath] || errors[wholePath]) && (
                <p className="m-0 mt-1 text-[11px] text-[var(--danger)]" role="alert">
                  {errors[wholePath] || errors[vendorPath] || errors[modelPath]}
                </p>
              )}
            </div>
          );
        })}
      </div>

      <div className="mt-2 grid gap-1.5 sm:grid-cols-[minmax(120px,0.6fr)_minmax(140px,0.7fr)_minmax(140px,1fr)_auto]">
        <input
          aria-label="New profile id"
          className={`${controlClass} mono`}
          value={newId}
          disabled={!canEdit}
          placeholder="profile-id"
          spellCheck={false}
          onChange={(event) => setNewId(event.currentTarget.value)}
        />
        <select
          aria-label="New profile vendor"
          className={controlClass}
          value={newVendor}
          disabled={!canEdit}
          onChange={(event) => setNewVendor(event.currentTarget.value)}
        >
          <option value="">Vendor (optional)</option>
          {AGENT_VENDOR_OPTIONS.map((option) => (
            <option key={option} value={option}>
              {option}
            </option>
          ))}
        </select>
        <input
          aria-label="New profile model"
          className={`${controlClass} mono`}
          value={newModel}
          disabled={!canEdit}
          placeholder={
            newVendor
              ? "Model (optional; type or pick from suggestions)"
              : "Model (optional)"
          }
          list={newVendor ? "new-profile-model-suggestions" : undefined}
          onChange={(event) => setNewModel(event.currentTarget.value)}
        />
        {newVendor ? (
          <NewProfileModelSuggestions vendor={newVendor} />
        ) : null}
        <Button variant="ghost" size="sm" disabled={!canEdit} onClick={stageNew}>
          Add profile
        </Button>
      </div>
      {!editableByAuthority ? (
        <p className="m-0 mt-1 text-[10px] text-[var(--text-muted)]">
          Agent profiles are read-only under the active config authority.
        </p>
      ) : null}
      {localError ? (
        <p className="m-0 mt-1 text-[11px] text-[var(--danger)]" role="alert">
          {localError}
        </p>
      ) : null}
    </div>
  );
}

function AgentEnvironment({
  data,
  secretSet,
  unsetPaths,
  errors,
  onSet,
  onRemove,
  onUndoRemove,
  onInputDirtyChange,
  disabled,
}: {
  data: ConfigData;
  secretSet: Record<string, string>;
  unsetPaths: Set<string>;
  errors: ErrorMap;
  onSet: (key: string, value: string) => void;
  onRemove: (key: string, existed: boolean) => void;
  onUndoRemove: (key: string) => void;
  onInputDirtyChange: (dirty: boolean) => void;
  disabled: boolean;
}) {
  const [key, setKey] = useState("");
  const [secret, setSecret] = useState("");
  const [localError, setLocalError] = useState<string | null>(null);
  const envMeta = data.metadata.fields["agent.env"];
  const editableByAuthority = metaIsEditable(envMeta);
  const canAdd = editableByAuthority && !disabled;
  const existingKeys = data.agent?.envKeys ?? [];
  const stagedKeys = Object.keys(secretSet)
    .filter((path) => path.startsWith("agent.env."))
    .map((path) => path.slice("agent.env.".length));
  const keys = [...new Set([...existingKeys, ...stagedKeys])].sort();

  const stageSecret = () => {
    const normalized = key.trim();
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(normalized)) {
      setLocalError("Use an environment-variable name such as OPENAI_API_KEY.");
      return;
    }
    const path = `agent.env.${normalized}`;
    if (
      existingKeys.includes(normalized) &&
      !metaIsEditable(data.metadata.fields[path] ?? envMeta)
    ) {
      setLocalError(`${normalized} is controlled by a higher-precedence authority and is read-only.`);
      return;
    }
    if (!secret) {
      setLocalError("Enter a secret value.");
      return;
    }
    onSet(normalized, secret);
    setKey("");
    setSecret("");
    onInputDirtyChange(false);
    setLocalError(null);
  };

  return (
    <div className="mt-2 border-t border-[var(--border)] pt-2" data-testid="agent-env">
      <div className="flex items-center justify-between gap-2">
        <div>
          <h3 className="m-0 text-[12px] font-medium">Agent environment</h3>
          <p className="m-0 text-[10px] text-[var(--text-muted)]">
            Values are write-only and are never returned by the daemon.
          </p>
        </div>
        <SourceBadge meta={envMeta} />
      </div>

      <div className="mt-2 flex flex-wrap gap-1.5">
        {keys.length === 0 ? (
          <span className="text-[11px] text-[var(--text-muted)]">
            No agent environment variables configured.
          </span>
        ) : null}
        {keys.map((envKey) => {
          const path = `agent.env.${envKey}`;
          const exists = existingKeys.includes(envKey);
          const pendingRemoval = unsetPaths.has(path);
          const pendingSet = Object.hasOwn(secretSet, path);
          const keyMeta = data.metadata.fields[path] ?? envMeta;
          const editable = metaIsEditable(keyMeta);
          return (
            <span
              key={envKey}
              className="inline-flex items-center gap-1 rounded border border-[var(--border)] bg-[var(--bg)] px-1.5 py-0.5 mono text-[11px]"
            >
              <span className={pendingRemoval ? "line-through opacity-60" : ""}>
                {envKey}
              </span>
              {pendingSet ? (
                <span className="text-[9px] text-[var(--accent)]">pending</span>
              ) : null}
              {pendingRemoval ? (
                <button
                  type="button"
                  className="border-0 bg-transparent p-0 text-[10px] text-[var(--accent)]"
                  disabled={disabled}
                  onClick={() => onUndoRemove(envKey)}
                >
                  undo
                </button>
              ) : (
                <button
                  type="button"
                  aria-label={`Remove ${envKey}`}
                  className="border-0 bg-transparent p-0 text-[12px] text-[var(--danger)] disabled:opacity-40"
                  disabled={disabled || !editable}
                  onClick={() => onRemove(envKey, exists)}
                >
                  ×
                </button>
              )}
              {errors[path] ? (
                <span className="text-[var(--danger)]" title={errors[path]}>
                  !
                </span>
              ) : null}
            </span>
          );
        })}
      </div>

      <div className="mt-2 grid gap-1.5 sm:grid-cols-[minmax(150px,0.7fr)_minmax(220px,1fr)_auto]">
        <input
          aria-label="Environment variable name"
          className={`${controlClass} mono`}
          value={key}
          disabled={!canAdd}
          placeholder="VARIABLE_NAME"
          autoCapitalize="characters"
          spellCheck={false}
          onChange={(event) => {
            const next = event.currentTarget.value;
            onInputDirtyChange(next.length > 0 || secret.length > 0);
            setKey(next);
          }}
        />
        <input
          aria-label="Environment variable secret"
          className={`${controlClass} mono`}
          type="password"
          value={secret}
          disabled={!canAdd}
          placeholder="Set or replace value"
          autoComplete="new-password"
          onChange={(event) => {
            const next = event.currentTarget.value;
            onInputDirtyChange(key.length > 0 || next.length > 0);
            setSecret(next);
          }}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              event.preventDefault();
              stageSecret();
            }
          }}
        />
        <Button
          variant="ghost"
          size="sm"
          disabled={!canAdd}
          onClick={stageSecret}
        >
          Stage secret
        </Button>
      </div>
      {!editableByAuthority ? (
        <p className="m-0 mt-1 text-[10px] text-[var(--text-muted)]">
          Agent environment is read-only under the active config authority.
        </p>
      ) : null}
      {localError ? (
        <p className="m-0 mt-1 text-[11px] text-[var(--danger)]" role="alert">
          {localError}
        </p>
      ) : null}
      {Object.entries(errors)
        .filter(([path]) => path.startsWith("agent.env."))
        .map(([path, message]) => (
          <p
            key={path}
            className="m-0 mt-1 text-[11px] text-[var(--danger)]"
            role="alert"
          >
            <code>{path}</code>: {message}
          </p>
        ))}
    </div>
  );
}

function ReloadWarning({ data }: { data: ConfigData }) {
  const { lastError, rejectedPaths = [], lastAttemptAt, lastAppliedAt } =
    data.metadata;
  if (!lastError && rejectedPaths.length === 0) return null;
  return (
    <div
      className="rounded border border-[var(--danger)] bg-[var(--bg-elevated)] px-3 py-2 text-[12px]"
      role="alert"
    >
      <p className="m-0 font-semibold text-[var(--danger)]">
        Latest config reload was rejected
      </p>
      <p className="m-0 mt-0.5 text-[var(--text-muted)]">
        The daemon is still using the last-known-good configuration
        {lastAppliedAt ? ` from ${formatTs(lastAppliedAt)}` : ""}.
      </p>
      {lastError ? (
        <pre className="m-0 mt-1 whitespace-pre-wrap break-words mono text-[11px] text-[var(--danger)]">
          {lastError}
        </pre>
      ) : null}
      {rejectedPaths.length ? (
        <div className="mt-1 flex flex-wrap gap-1">
          {rejectedPaths.map((path) => (
            <code
              key={path}
              className="rounded border border-[var(--border)] px-1 py-px text-[10px]"
            >
              {path}
            </code>
          ))}
        </div>
      ) : null}
      {lastAttemptAt ? (
        <p className="m-0 mt-1 mono text-[10px] text-[var(--text-muted)]">
          attempted {formatTs(lastAttemptAt)}
        </p>
      ) : null}
    </div>
  );
}

function AdvancedGroupSection({
  group,
  paths,
  dirtyCount,
  data,
  secretSet,
  unsetPaths,
  errors,
  environmentResetToken,
  onSecretSet,
  onSecretRemove,
  onSecretUndoRemove,
  onEnvironmentInputDirtyChange,
  disabled,
  renderField,
}: {
  group: ConfigGroup;
  paths: string[];
  dirtyCount: number;
  data: ConfigData;
  secretSet: Record<string, string>;
  unsetPaths: Set<string>;
  errors: ErrorMap;
  environmentResetToken: number;
  onSecretSet: (key: string, value: string) => void;
  onSecretRemove: (key: string, existed: boolean) => void;
  onSecretUndoRemove: (key: string) => void;
  onEnvironmentInputDirtyChange: (dirty: boolean) => void;
  disabled: boolean;
  renderField: (path: string) => ReactNode;
}) {
  // Agent group keeps its curated environment editor even when it has no plain
  // hot-editable leaves left (all leaves either promoted to Essentials or
  // filtered out as write-only).
  const hasEnv = group.id === "agent";

  const [open, setOpen] = useState<boolean>(dirtyCount > 0);
  // Auto-open when this group's dirty count transitions to positive.
  const prevDirtyRef = useRef<number>(dirtyCount);
  useEffect(() => {
    if (prevDirtyRef.current === 0 && dirtyCount > 0) setOpen(true);
    prevDirtyRef.current = dirtyCount;
  }, [dirtyCount]);

  if (paths.length === 0 && !hasEnv) return null;

  const total = paths.length + (hasEnv ? 1 : 0);
  const meta = `${total} setting${total === 1 ? "" : "s"}${
    dirtyCount > 0 ? ` · ${dirtyCount} unsaved` : ""
  }`;

  return (
    <details
      className={`rounded border border-[var(--border)] bg-[var(--bg-elevated)] [&_summary::-webkit-details-marker]:hidden${
        group.id === "roles" ? " xl:col-span-2" : ""
      }`}
      data-config-group={group.id}
      data-testid={`config-advanced-${group.id}`}
      open={open}
      onToggle={(event) => setOpen(event.currentTarget.open)}
    >
      <summary
        aria-controls={`config-adv-body-${group.id}`}
        className="flex cursor-pointer list-none items-center justify-between gap-2 px-3 py-1.5 hover:bg-[var(--bg-muted)]"
      >
        <span className="flex items-center gap-2">
          <span
            aria-hidden
            className="inline-block w-2 text-[10px] text-[var(--text-muted)]"
          >
            {open ? "▾" : "▸"}
          </span>
          <span className="text-[12px] font-semibold tracking-wide uppercase text-[var(--text)]">
            {group.title}
          </span>
          {dirtyCount > 0 ? (
            <span
              className="rounded border border-[var(--warn)] px-1 py-px mono text-[10px] text-[var(--warn)]"
              title={`${dirtyCount} unsaved in ${group.title}`}
            >
              {dirtyCount}
            </span>
          ) : null}
        </span>
        <span className="mono text-[10px] text-[var(--text-muted)]">
          {meta}
        </span>
      </summary>
      <div id={`config-adv-body-${group.id}`} className="px-3 py-2">
        <p className="m-0 mb-1 text-[11px] text-[var(--text-muted)]">
          {group.description}
        </p>
        <div>{paths.map(renderField)}</div>
        {hasEnv ? (
          <AgentEnvironment
            key={environmentResetToken}
            data={data}
            secretSet={secretSet}
            unsetPaths={unsetPaths}
            errors={errors}
            onSet={onSecretSet}
            onRemove={onSecretRemove}
            onUndoRemove={onSecretUndoRemove}
            onInputDirtyChange={onEnvironmentInputDirtyChange}
            disabled={disabled}
          />
        ) : null}
      </div>
    </details>
  );
}

function canKeepStagedUnset(
  data: ConfigData,
  path: string,
  meta: ConfigFieldMetadata | undefined,
): boolean {
  if (sourceIsConfigFile(meta?.source)) return true;
  // Curated profile/role-agent paths may lack direct metadata entries and fall
  // back to default/hot meta; keep unsets when the published value still exists.
  if (isAgentProfileWholePath(path)) {
    const id = path.slice("agent.profiles.".length);
    return data.agent?.profiles?.[id] != null;
  }
  if (isAgentProfileLeafPath(path) || isRoleAgentLeafPath(path)) {
    return getConfigValue(data, path) !== undefined;
  }
  return false;
}

function reconcilePendingAfterRebase(
  next: ConfigData,
  drafts: Record<string, ConfigDraft>,
  secretSet: Record<string, string>,
  unsetPaths: Set<string>,
) {
  const nextDrafts: Record<string, ConfigDraft> = {};
  let matchedPublished = 0;
  let noLongerEditable = 0;
  for (const [path, draft] of Object.entries(drafts)) {
    const meta = resolveFieldMeta(next, path);
    if (!metaIsEditable(meta)) {
      noLongerEditable += 1;
      continue;
    }
    // Include unset-only empty profile/role drafts (no set/error).
    if (!draftStagesConfigChange(next, path, draft)) {
      matchedPublished += 1;
      continue;
    }
    nextDrafts[path] = draft;
  }

  const nextUnsetPaths = new Set<string>();
  let clearedWriteOnly = Object.keys(secretSet).length;
  for (const path of unsetPaths) {
    if (path.startsWith("agent.env.")) {
      clearedWriteOnly += 1;
      continue;
    }
    const meta = resolveFieldMeta(next, path);
    if (!metaIsEditable(meta) || !canKeepStagedUnset(next, path, meta)) {
      // Whole-profile and curated identity unsets without a live value are
      // treated as matched/cleared rather than "no longer editable".
      if (isCuratedAgentIdentityPath(path) && !canKeepStagedUnset(next, path, meta)) {
        matchedPublished += 1;
        continue;
      }
      noLongerEditable += 1;
      continue;
    }
    nextUnsetPaths.add(path);
  }

  const notices: string[] = [];
  if (clearedWriteOnly > 0) {
    notices.push(
      "Write-only agent environment changes were cleared; review the current keys and restage them.",
    );
  }
  if (matchedPublished > 0) {
    notices.push(
      `${matchedPublished} pending ${matchedPublished === 1 ? "change now matches" : "changes now match"} the published configuration and ${matchedPublished === 1 ? "was" : "were"} cleared.`,
    );
  }
  if (noLongerEditable > 0) {
    notices.push(
      `${noLongerEditable} pending ${noLongerEditable === 1 ? "change is" : "changes are"} no longer editable and ${noLongerEditable === 1 ? "was" : "were"} cleared.`,
    );
  }

  return {
    drafts: nextDrafts,
    secretSet: {} as Record<string, string>,
    unsetPaths: nextUnsetPaths,
    notice: notices.join(" "),
  };
}

export function ConfigPage() {
  const toast = useToast();
  const [data, setData] = useState<ConfigData | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saveConflict, setSaveConflict] = useState(false);
  const [rebaseNotice, setRebaseNotice] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [drafts, setDrafts] = useState<Record<string, ConfigDraft>>({});
  const [secretSet, setSecretSet] = useState<Record<string, string>>({});
  const [unsetPaths, setUnsetPaths] = useState<Set<string>>(new Set());
  const [fieldErrors, setFieldErrors] = useState<ErrorMap>({});
  const [confirmChanges, setConfirmChanges] = useState<HighImpactChange[]>([]);
  const [confirmBody, setConfirmBody] = useState<PatchConfigBody | null>(null);
  const [environmentInputDirty, setEnvironmentInputDirty] = useState(false);
  const [environmentResetToken, setEnvironmentResetToken] = useState(0);
  const loadAbort = useRef<AbortController | null>(null);
  const dataRef = useRef<ConfigData | null>(null);
  const draftsRef = useRef(drafts);
  const secretSetRef = useRef(secretSet);
  const unsetPathsRef = useRef(unsetPaths);
  const conflictRevisionRef = useRef<string | null>(null);
  dataRef.current = data;
  draftsRef.current = drafts;
  secretSetRef.current = secretSet;
  unsetPathsRef.current = unsetPaths;

  const load = useCallback(async (rebaseDrafts = false) => {
    loadAbort.current?.abort();
    const controller = new AbortController();
    loadAbort.current = controller;
    setLoading(true);
    try {
      const next = await fetchConfig(controller.signal);
      if (controller.signal.aborted) return;
      setData(next);
      setLoadError(null);
      if (rebaseDrafts) {
        // Stay locked only when the published generation is unchanged AND the
        // daemon still reports a rejected reload. Same revision without
        // lastError means the accepted file is back (or never left); OCC will
        // re-check on the next PATCH, so unlock and let the operator retry.
        if (
          conflictRevisionRef.current !== null &&
          next.metadata.revision === conflictRevisionRef.current &&
          Boolean(next.metadata.lastError)
        ) {
          setSaveConflict(true);
          setSaveError(
            "The changed config file is still rejected. Repair it outside the dashboard, wait for a successful reload, then try again.",
          );
          setFieldErrors({});
          return;
        }
        const reconciled = reconcilePendingAfterRebase(
          next,
          draftsRef.current,
          secretSetRef.current,
          unsetPathsRef.current,
        );
        setDrafts(reconciled.drafts);
        setSecretSet(reconciled.secretSet);
        setUnsetPaths(reconciled.unsetPaths);
        setRebaseNotice(reconciled.notice || null);
        setEnvironmentInputDirty(false);
        setEnvironmentResetToken((current) => current + 1);
        setSaveError(null);
        setFieldErrors({});
        setSaveConflict(false);
        conflictRevisionRef.current = null;
      } else {
        setRebaseNotice(null);
        setEnvironmentInputDirty(false);
        setEnvironmentResetToken((current) => current + 1);
      }
    } catch (error) {
      if (controller.signal.aborted) return;
      setLoadError(error instanceof Error ? error.message : String(error));
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load(false);
    return () => loadAbort.current?.abort();
  }, [load]);

  const retireLoad = useCallback(() => {
    loadAbort.current?.abort();
    loadAbort.current = null;
    setLoading(false);
  }, []);

  const onEnvironmentInputDirtyChange = useCallback(
    (dirty: boolean) => {
      if (dirty) retireLoad();
      setEnvironmentInputDirty(dirty);
    },
    [retireLoad],
  );

  const patch = useMemo(
    () =>
      data
        ? buildConfigPatch(data, drafts, unsetPaths, secretSet)
        : { body: { revision: "", set: {}, unset: [] }, errors: {} },
    [data, drafts, secretSet, unsetPaths],
  );
  const dirtyCount =
    Object.keys(patch.body.set).length +
    patch.body.unset.length +
    Object.keys(patch.errors).length;
  const formDirtyCount = dirtyCount + (environmentInputDirty ? 1 : 0);
  const editorLocked = saving || confirmBody !== null || saveConflict;

  const clearPathError = useCallback((path: string) => {
    setFieldErrors((current) => {
      if (!Object.hasOwn(current, path)) return current;
      const next = { ...current };
      delete next[path];
      return next;
    });
    setSaveError(null);
  }, []);

  const onDraft = useCallback(
    (path: string, value: ConfigDraft) => {
      retireLoad();
      setDrafts((current) => {
        if (data) {
          // Retain empty profile/role .model/.profile drafts that stage only an
          // unset (no set/error). Dropping them snaps the control back and
          // leaves Save with no unset until the separate Unset button is used.
          if (!draftStagesConfigChange(data, path, value)) {
            if (!Object.hasOwn(current, path)) return current;
            const next = { ...current };
            delete next[path];
            return next;
          }
        }
        return { ...current, [path]: value };
      });
      setUnsetPaths((current) => {
        if (!current.has(path)) return current;
        const next = new Set(current);
        next.delete(path);
        return next;
      });
      clearPathError(path);
    },
    [clearPathError, data, retireLoad],
  );

  const onToggleUnset = useCallback(
    (path: string) => {
      retireLoad();
      setDrafts((current) => {
        if (!Object.hasOwn(current, path)) return current;
        const next = { ...current };
        delete next[path];
        return next;
      });
      setUnsetPaths((current) => {
        const next = new Set(current);
        if (next.has(path)) next.delete(path);
        else next.add(path);
        return next;
      });
      clearPathError(path);
    },
    [clearPathError, retireLoad],
  );

  // Global agent.model: restore absence (null). When the leaf is already
  // absent/default-sourced, FieldFrame has no Unset and Inherit is disabled —
  // clearing the draft is the only field-local path back to unbound (so
  // params/CLI --model may still apply). When a binding is published, stage
  // unset instead of set "".
  const onRestoreModelAbsence = useCallback(
    (path: string) => {
      retireLoad();
      const published = data ? getConfigValue(data, path) : undefined;
      setDrafts((current) => {
        if (!Object.hasOwn(current, path)) return current;
        const next = { ...current };
        delete next[path];
        return next;
      });
      setUnsetPaths((current) => {
        const next = new Set(current);
        if (published == null) {
          next.delete(path);
        } else {
          next.add(path);
        }
        return next;
      });
      clearPathError(path);
    },
    [clearPathError, data, retireLoad],
  );

  const onSecretSet = useCallback(
    (key: string, value: string) => {
      retireLoad();
      const path = `agent.env.${key}`;
      setSecretSet((current) => ({ ...current, [path]: value }));
      setUnsetPaths((current) => {
        if (!current.has(path)) return current;
        const next = new Set(current);
        next.delete(path);
        return next;
      });
      clearPathError(path);
    },
    [clearPathError, retireLoad],
  );

  const onSecretRemove = useCallback(
    (key: string, existed: boolean) => {
      retireLoad();
      const path = `agent.env.${key}`;
      setSecretSet((current) => {
        if (!Object.hasOwn(current, path)) return current;
        const next = { ...current };
        delete next[path];
        return next;
      });
      if (existed) {
        setUnsetPaths((current) => new Set(current).add(path));
      }
      clearPathError(path);
    },
    [clearPathError, retireLoad],
  );

  const onSecretUndoRemove = useCallback(
    (key: string) => {
      retireLoad();
      const path = `agent.env.${key}`;
      setUnsetPaths((current) => {
        const next = new Set(current);
        next.delete(path);
        return next;
      });
      clearPathError(path);
    },
    [clearPathError, retireLoad],
  );

  const onProfileRemove = useCallback(
    (id: string, existed: boolean) => {
      retireLoad();
      const wholePath = `agent.profiles.${id}`;
      const vendorPath = agentProfilePath(id, "vendor");
      const modelPath = agentProfilePath(id, "model");
      setDrafts((current) => {
        if (
          !Object.hasOwn(current, vendorPath) &&
          !Object.hasOwn(current, modelPath)
        ) {
          return current;
        }
        const next = { ...current };
        delete next[vendorPath];
        delete next[modelPath];
        return next;
      });
      setUnsetPaths((current) => {
        const next = new Set(current);
        next.delete(vendorPath);
        next.delete(modelPath);
        if (existed) next.add(wholePath);
        else next.delete(wholePath);
        return next;
      });
      clearPathError(wholePath);
      clearPathError(vendorPath);
      clearPathError(modelPath);
    },
    [clearPathError, retireLoad],
  );

  const onProfileUndoRemove = useCallback(
    (id: string) => {
      retireLoad();
      const wholePath = `agent.profiles.${id}`;
      setUnsetPaths((current) => {
        if (!current.has(wholePath)) return current;
        const next = new Set(current);
        next.delete(wholePath);
        return next;
      });
      clearPathError(wholePath);
    },
    [clearPathError, retireLoad],
  );

  const persist = useCallback(
    async (body: PatchConfigBody) => {
      // A refresh started while the form was still clean may still be in flight
      // after the user edits and saves. Retire it before PATCH so its older
      // snapshot cannot overwrite the authoritative PATCH response.
      retireLoad();
      setSaving(true);
      setSaveConflict(false);
      setSaveError(null);
      setFieldErrors({});
      try {
        const applied = await patchConfig(body);
        // PATCH returns the authoritative normalized snapshot from the same
        // publication boundary. Using it avoids turning a later GET failure into
        // a false "save failed" result after the file was already replaced.
        setData(applied);
        setLoadError(null);
        setDrafts({});
        setSecretSet({});
        setUnsetPaths(new Set());
        setConfirmBody(null);
        setConfirmChanges([]);
        setRebaseNotice(null);
        setEnvironmentInputDirty(false);
        setEnvironmentResetToken((current) => current + 1);
        conflictRevisionRef.current = null;
        toast.success("Configuration saved and applied to new runs.");
      } catch (error) {
        setConfirmBody(null);
        setConfirmChanges([]);
        const byField = configFieldErrors(error);
        setFieldErrors(byField);
        setSaveError(error instanceof Error ? error.message : String(error));
        const conflict = error instanceof ApiError && error.status === 409;
        setSaveConflict(conflict);
        if (conflict) {
          conflictRevisionRef.current = dataRef.current?.metadata.revision ?? body.revision;
        }
        toast.error(error instanceof Error ? error.message : String(error));
      } finally {
        setSaving(false);
      }
    },
    [retireLoad, toast],
  );

  const requestSave = useCallback(() => {
    if (!data) return;
    if (saveConflict || environmentInputDirty) return;
    if (Object.keys(patch.errors).length > 0) {
      setFieldErrors(patch.errors);
      setSaveError("Correct the highlighted fields before saving.");
      return;
    }
    if (Object.keys(patch.body.set).length === 0 && patch.body.unset.length === 0) {
      toast.info("No configuration changes to save.");
      return;
    }
    const impact = highImpactChanges(data, patch.body.set, patch.body.unset);
    if (impact.length > 0) {
      setConfirmChanges(impact);
      setConfirmBody(patch.body);
      return;
    }
    void persist(patch.body);
  }, [data, environmentInputDirty, patch, persist, saveConflict, toast]);

  const discard = useCallback(() => {
    setDrafts({});
    setSecretSet({});
    setUnsetPaths(new Set());
    setFieldErrors({});
    setSaveError(null);
    setSaveConflict(false);
    setRebaseNotice(null);
    setEnvironmentInputDirty(false);
    setEnvironmentResetToken((current) => current + 1);
    setConfirmBody(null);
    setConfirmChanges([]);
    conflictRevisionRef.current = null;
  }, []);

  if (loading && !data) {
    return <p className="m-0 text-[12px] text-[var(--text-muted)]">Loading configuration…</p>;
  }
  if (loadError && !data) {
    return <PanelError message={loadError} onRetry={() => void load(false)} />;
  }
  if (!data) return null;

  // Renders one editable field row via FieldFrame + ConfigControl.
  const renderField = (path: string) => {
    const effective = getConfigValue(data, path);
    const kind = configFieldKind(path, effective);
    const value = Object.hasOwn(drafts, path)
      ? drafts[path]
      : draftFromValue(kind, effective);
    const meta = resolveFieldMeta(data, path);
    const dirty = Object.hasOwn(drafts, path);
    const unset = unsetPaths.has(path);
    const modelScope = agentModelScope(path);
    // Model leaves: keep absence (null) distinct from explicit "" suppress.
    // Profile/role: null → Inherit. Global: null → unbound (params/CLI may
    // still supply --model); only "" is Vendor default suppress.
    const modelValue: string | null = unset
      ? null
      : Object.hasOwn(drafts, path)
        ? String(drafts[path] ?? "")
        : effective == null
          ? null
          : String(effective);
    return (
      <FieldFrame
        key={path}
        path={path}
        meta={meta}
        error={fieldErrors[path]}
        dirty={dirty}
        unset={unset}
        publishedValue={effective}
        disabled={editorLocked}
        onUnset={() => onToggleUnset(path)}
      >
        {modelScope ? (
          <ModelCombobox
            id={`config-${path}`}
            ariaLabel={path}
            vendor={effectiveAgentVendor(data, drafts, unsetPaths, modelScope)}
            value={modelValue}
            unset={unset}
            disabled={editorLocked || !metaIsEditable(meta)}
            allowInherit={modelScope.kind !== "global"}
            placeholder={
              modelScope.kind === "global"
                ? "Model id (empty = vendor default / suppress)"
                : "Model id (empty = vendor default; Inherit = previous layer)"
            }
            onCommitValue={(next) => onDraft(path, next)}
            onInherit={
              modelScope.kind === "global" ? undefined : () => onToggleUnset(path)
            }
            onUnbound={
              modelScope.kind === "global"
                ? () => onRestoreModelAbsence(path)
                : undefined
            }
          />
        ) : (
          <ConfigControl
            path={path}
            kind={kind}
            value={value}
            options={configSelectOptions(path)}
            disabled={editorLocked || !metaIsEditable(meta)}
            unset={unset}
            onChange={(next) => onDraft(path, next)}
          />
        )}
      </FieldFrame>
    );
  };

  // Curated essentials: only paths this snapshot actually exposes as
  // hot-editable, in the intended display order. Anything else falls under
  // Advanced. Role agent leaves render in a separate curated block below.
  const groupPathSets = CONFIG_GROUPS.map((group) => ({
    group,
    paths: configFieldPaths(data, group),
  }));
  const knownGroupPaths = new Set<string>();
  for (const { paths } of groupPathSets) for (const p of paths) knownGroupPaths.add(p);
  const essentialPaths = ESSENTIAL_PATHS.filter((p) => knownGroupPaths.has(p));

  // Advanced paths per group: everything hot-editable that isn't essential and
  // isn't a curated role-agent leaf (rendered under Essentials).
  const advancedByGroup: Record<string, string[]> = {};
  for (const { group, paths } of groupPathSets) {
    advancedByGroup[group.id] = paths.filter(
      (p) => !isEssentialConfigPath(p),
    );
  }

  // Dirty counts per advanced group — for auto-open + collapsed badge.
  const dirtyPaths = new Set<string>([
    ...Object.keys(drafts),
    ...Array.from(unsetPaths),
    ...Object.keys(secretSet),
  ]);
  const advancedDirtyByGroup: Record<string, number> = {};
  let essentialsDirty = 0;
  for (const path of dirtyPaths) {
    if (isEssentialConfigPath(path)) {
      essentialsDirty += 1;
      continue;
    }
    if (isAgentProfileLeafPath(path) || isAgentProfileWholePath(path)) {
      // Profile edits are surfaced via the Essentials Agent Profiles block.
      essentialsDirty += 1;
      continue;
    }
    for (const { group } of groupPathSets) {
      if (group.accepts(path)) {
        advancedDirtyByGroup[group.id] =
          (advancedDirtyByGroup[group.id] ?? 0) + 1;
        break;
      }
    }
  }
  // environmentInputDirty is a typed-but-unstaged flag; count it under Agent.
  if (environmentInputDirty) {
    advancedDirtyByGroup.agent = (advancedDirtyByGroup.agent ?? 0) + 1;
  }

  // Keep dock while dirty (including conflict): Save stays disabled via
  // editorLocked; Discard remains available. Conflict banner owns reload.
  const dockVisible = formDirtyCount > 0;

  return (
    <div className={`flex flex-col gap-3 ${dockVisible ? "pb-24" : ""}`}>
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <h1 className="m-0 inline-flex items-center gap-1.5 text-[15px] font-semibold">
            <Settings
              size={15}
              className="shrink-0 text-[var(--text-muted)]"
              aria-hidden
            />
            Configuration
          </h1>
          <p className="m-0 mt-0.5 text-[11px] text-[var(--text-muted)]">
            Hot-safe global policy. Common settings are shown first; the rest
            live under Advanced. Changes apply to new runs only.
          </p>
        </div>
        <div className="flex items-center gap-1.5">
          {formDirtyCount > 0 ? (
            <span
              className="mono text-[11px] text-[var(--warn)]"
              data-testid="config-dirty-count"
            >
              {formDirtyCount} unsaved
            </span>
          ) : null}
          <Button
            variant="ghost"
            size="sm"
            className="gap-1.5"
            disabled={editorLocked || formDirtyCount > 0}
            onClick={() => void load(false)}
            title={
              formDirtyCount > 0
                ? "Discard or save pending changes before refreshing"
                : undefined
            }
          >
            <RefreshCw size={13} className="shrink-0" aria-hidden />
            Refresh
          </Button>
        </div>
      </div>

      {environmentInputDirty ? (
        <div
          className="rounded border border-[var(--warn)] px-3 py-2 text-[12px] text-[var(--warn)]"
          role="status"
        >
          Stage the agent environment value or discard it before saving or
          refreshing.
        </div>
      ) : null}

      <ReloadWarning data={data} />

      {/* Compact single-line source strip — was a heavy 4-col card. */}
      <div
        className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded border border-[var(--border)] bg-[var(--bg-elevated)] px-3 py-1.5 text-[11px] text-[var(--text-muted)]"
        aria-label="Configuration source"
      >
        <span>
          <span className="uppercase tracking-wide">Source</span>
        </span>
        <span
          className="mono max-w-full truncate text-[var(--text)]"
          title={data.metadata.configPath}
        >
          {data.metadata.configPath || "—"}
        </span>
        <span className="mono">
          {data.metadata.format || "—"} ·{" "}
          {data.metadata.filePresent ? "present" : "not created"}
        </span>
        <span className="mono">
          applied {formatTs(data.metadata.lastAppliedAt)}
        </span>
        <span className="mono">
          attempted {formatTs(data.metadata.lastAttemptAt)}
        </span>
      </div>

      {loadError ? (
        <PanelError
          message={loadError}
          onRetry={
            formDirtyCount === 0 && !editorLocked
              ? () => void load(false)
              : undefined
          }
        />
      ) : null}
      {saveConflict ? (
        <div
          className="rounded border border-[var(--warn)] px-3 py-2 text-[12px]"
          role="alert"
        >
          <p className="m-0 text-[var(--warn)]">
            The file changed after this form loaded. Reload the published
            snapshot and keep your pending edits rebased on it, then review each
            published value before saving again.
          </p>
          <Button
            variant="ghost"
            size="sm"
            className="mt-1"
            disabled={loading || saving || confirmBody !== null}
            onClick={() => void load(true)}
          >
            Reload latest and keep edits
          </Button>
        </div>
      ) : null}
      {saveError ? (
        <div
          className="rounded border border-[var(--danger)] px-3 py-2 text-[12px] text-[var(--danger)]"
          role="alert"
        >
          {saveError}
        </div>
      ) : null}
      {rebaseNotice ? (
        <div
          className="rounded border border-[var(--warn)] px-3 py-2 text-[12px] text-[var(--warn)]"
          role="status"
        >
          {rebaseNotice}
        </div>
      ) : null}

      {/* Essentials — curated common knobs, always expanded. */}
      <Card title="Common settings" data-testid="config-essentials">
        <p className="m-0 mb-1 text-[11px] text-[var(--text-muted)]">
          The knobs most operators tune. Everything else is under Advanced
          below.
        </p>
        <div>{essentialPaths.map(renderField)}</div>

        <AgentProfiles
          data={data}
          drafts={drafts}
          unsetPaths={unsetPaths}
          errors={fieldErrors}
          onDraft={onDraft}
          onToggleUnset={onToggleUnset}
          onRemoveProfile={onProfileRemove}
          onUndoRemoveProfile={onProfileUndoRemove}
          disabled={editorLocked}
        />

        <div
          className="mt-2 border-t border-[var(--border)] pt-2"
          data-testid="role-agent-bindings"
        >
          <div>
            <h3 className="m-0 text-[12px] font-medium">Role agent bindings</h3>
            <p className="m-0 text-[10px] text-[var(--text-muted)]">
              Optional profile / vendor / model override per coding role. Leave
              blank to inherit the global agent.
            </p>
          </div>
          <div className="mt-1.5 grid gap-2 xl:grid-cols-2">
            {CODING_ROLES.map((role: CodingRole) => (
              <div
                key={role}
                className="rounded border border-[var(--border)] px-2 py-1.5"
                data-config-group={`roles.${role}.agent`}
              >
                <div className="mb-1 text-[11px] font-medium capitalize">
                  {role}
                </div>
                <div>
                  {ROLE_AGENT_FIELDS.map((field) =>
                    renderField(roleAgentPath(role, field)),
                  )}
                </div>
              </div>
            ))}
          </div>
        </div>
      </Card>

      {/* Advanced — everything else, grouped and collapsed. */}
      <section
        aria-labelledby="config-advanced-heading"
        data-testid="config-advanced"
      >
        <div className="mb-1.5 flex items-baseline justify-between">
          <h2
            id="config-advanced-heading"
            className="m-0 text-[12px] font-semibold uppercase tracking-wide text-[var(--text-muted)]"
          >
            Advanced
          </h2>
          <span className="text-[11px] text-[var(--text-muted)]">
            Expand a section to reveal less-tuned knobs.
          </span>
        </div>
        <div className="grid items-start gap-2 xl:grid-cols-2">
          {CONFIG_GROUPS.map((group) => (
            <AdvancedGroupSection
              key={group.id}
              group={group}
              paths={advancedByGroup[group.id] ?? []}
              dirtyCount={advancedDirtyByGroup[group.id] ?? 0}
              data={data}
              secretSet={secretSet}
              unsetPaths={unsetPaths}
              errors={fieldErrors}
              environmentResetToken={environmentResetToken}
              onSecretSet={onSecretSet}
              onSecretRemove={onSecretRemove}
              onSecretUndoRemove={onSecretUndoRemove}
              onEnvironmentInputDirtyChange={onEnvironmentInputDirtyChange}
              disabled={editorLocked}
              renderField={renderField}
            />
          ))}
        </div>
      </section>

      {/* Viewport-fixed save dock — reliably visible while scrolling. */}
      {dockVisible ? (
        <div
          className="fixed bottom-0 left-0 right-0 z-30 border-t border-[var(--border)] bg-[color-mix(in_srgb,var(--bg-elevated)_92%,transparent)] px-3 py-2 shadow-[0_-8px_24px_rgba(0,0,0,0.35)] backdrop-blur"
          role="region"
          aria-label="Unsaved configuration actions"
          data-testid="config-save-dock"
        >
          <div className="flex flex-wrap items-center justify-between gap-2">
            <p className="m-0 text-[12px] text-[var(--warn)]">
              {formDirtyCount} unsaved{" "}
              {formDirtyCount === 1 ? "change" : "changes"} — save to apply to
              new runs
              {environmentInputDirty
                ? " (stage or clear agent environment inputs first)"
                : ""}
              .
            </p>
            <div className="flex items-center gap-1.5">
              <Button
                variant="ghost"
                size="sm"
                disabled={saving || confirmBody !== null}
                onClick={discard}
              >
                Discard
              </Button>
              <Button
                size="sm"
                disabled={
                  editorLocked || environmentInputDirty || dirtyCount === 0
                }
                onClick={requestSave}
                title={
                  environmentInputDirty
                    ? "Stage or clear the agent environment inputs first"
                    : dirtyCount === 0
                      ? "No unsaved field changes"
                      : "Write changes to the config file and apply to new runs"
                }
              >
                {saving ? "Saving…" : "Save changes"}
              </Button>
            </div>
          </div>
        </div>
      ) : null}

      <ConfirmDialog
        open={confirmBody !== null}
        title="Confirm high-impact configuration"
        confirmLabel="Apply changes"
        danger
        busy={saving}
        onCancel={() => {
          if (!saving) {
            setConfirmBody(null);
            setConfirmChanges([]);
          }
        }}
        onConfirm={() => {
          if (confirmBody) void persist(confirmBody);
        }}
      >
        <p className="m-0">
          These changes allow Looper to make or publish consequential decisions:
        </p>
        <ul className="m-0 mt-1 list-disc pl-4">
          {confirmChanges.map((change) => (
            <li key={change.path}>
              {change.label} <code className="text-[10px] text-[var(--text-muted)]">{change.path}</code>
            </li>
          ))}
        </ul>
        <p className="m-0 mt-1 text-[var(--text-muted)]">
          The new policy applies only to runs started after the reload.
        </p>
      </ConfirmDialog>
    </div>
  );
}
