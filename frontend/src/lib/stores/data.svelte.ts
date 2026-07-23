import {
  DataService,
  type DbProjectInventory,
  type DbProjectInventoryRow,
} from "../api/generated/index";
import { callGenerated, isAbortError } from "../api/runtime.js";
import { m } from "../i18n/index.js";
import { router } from "./router.svelte.js";
import { LatestRead } from "../utils/latest-read.js";

export type DataView = "inventory" | "rules";

class DataStore {
  inventory: DbProjectInventory | null = $state(null);
  loading: boolean = $state(false);
  error: string = $state("");
  view: DataView = $state("inventory");
  selectedProjectKey: string = $state("");
  rulesMachine: string = $state("");

  #inventoryRead = new LatestRead();
  #loadVersion = 0;

  /**
   * The inventory row matching selectedProjectKey, or null when there is no
   * selection or the inventory has not loaded yet.
   */
  get selectedRow(): DbProjectInventoryRow | null {
    if (!this.selectedProjectKey || !this.inventory) return null;
    const rows = (this.inventory.projects ?? []) as DbProjectInventoryRow[];
    return rows.find((row) => row.project_key === this.selectedProjectKey) ?? null;
  }

  /** True once the inventory has loaded, a key is selected, but no row matches it. */
  get unknownProjectKey(): boolean {
    return this.inventory !== null && this.selectedProjectKey !== "" && this.selectedRow === null;
  }

  /**
   * Register a mounted DataPage so popstate re-hydrates and reloads while it is
   * on screen, mirroring the activity store's attach pattern. Returns a detach
   * callback for the component's onMount cleanup.
   */
  attach(): () => void {
    this.hydrateFromUrl(router.params);
    const onPop = () => {
      this.hydrateFromUrl(router.params);
      void this.load();
    };
    window.addEventListener("popstate", onPop);
    let detached = false;
    return () => {
      if (detached) return;
      detached = true;
      window.removeEventListener("popstate", onPop);
    };
  }

  /**
   * Replace view/selection state from URL query params. `view=rules` selects
   * the rules view and clears any project selection; any other value
   * (including absent or unknown) falls back to the inventory view with the
   * `project_key` param, if any, selected.
   */
  hydrateFromUrl(params: Record<string, string>) {
    if (params.view === "rules") {
      this.view = "rules";
      this.rulesMachine = params.machine ?? "";
      this.selectedProjectKey = "";
      return;
    }
    this.view = "inventory";
    this.rulesMachine = "";
    this.selectedProjectKey = params.project_key ?? "";
  }

  /** Write the current view/selection state to the URL through the router. */
  writeUrl() {
    const p: Record<string, string> = {};
    if (this.view === "rules") {
      p.view = "rules";
      if (this.rulesMachine) p.machine = this.rulesMachine;
    } else if (this.selectedProjectKey) {
      p.project_key = this.selectedProjectKey;
    }
    router.replaceParams(p);
  }

  /**
   * Load the project inventory. A background load keeps the current inventory
   * on screen and does not toggle `loading`; a foreground load clears `error`
   * and shows the loading state. A stale response (superseded by a newer
   * `load()` call) is dropped and never overwrites newer state.
   */
  async load(opts: { background?: boolean } = {}): Promise<boolean> {
    const signal = this.#inventoryRead.begin();
    const version = ++this.#loadVersion;
    if (!opts.background) this.loading = true;
    this.error = "";
    try {
      const inventory = await callGenerated(() => DataService.getApiV1DataProjects(), signal);
      if (!this.#inventoryRead.isCurrent(signal) || version !== this.#loadVersion) return false;
      this.inventory = inventory;
      return true;
    } catch (error) {
      if (isAbortError(error) || version !== this.#loadVersion) return false;
      // A failed background refresh keeps the current inventory on screen so a
      // transient blip does not blank an already-populated view. With no
      // inventory yet (a first load still failing) there is nothing to
      // preserve, so fall through and surface the error.
      if (opts.background && this.inventory !== null) return false;
      this.error = error instanceof Error ? error.message : m.data_load_failed();
      return false;
    } finally {
      if (this.#inventoryRead.finish(signal) && version === this.#loadVersion) {
        this.loading = false;
      }
    }
  }

  /** Cancel any in-flight inventory load and clear the loading flag. */
  cancelInFlightReads(): void {
    this.#loadVersion++;
    this.#inventoryRead.cancel();
    this.loading = false;
  }

  /** Select a project, switching to the inventory view. */
  selectProject(key: string) {
    this.view = "inventory";
    this.rulesMachine = "";
    this.selectedProjectKey = key;
    this.writeUrl();
  }

  /** Clear the current project selection without changing the view. */
  clearSelection() {
    this.selectedProjectKey = "";
    this.writeUrl();
  }

  /** Switch to the rules view, clearing any project selection. */
  showRules(machine = "") {
    this.view = "rules";
    this.selectedProjectKey = "";
    this.rulesMachine = machine;
    this.writeUrl();
  }

  /** Switch to the inventory view, clearing the rules machine. */
  showInventory() {
    this.view = "inventory";
    this.rulesMachine = "";
    this.writeUrl();
  }

  /** Update the rules-view machine filter in place. */
  setRulesMachine(machine: string) {
    this.rulesMachine = machine;
    this.writeUrl();
  }

  /**
   * Refresh the inventory after an apply mutation (e.g. a reclassification or
   * rule apply) that may have renamed or removed the originally selected
   * project. If the original key still exists, the selection is left as-is.
   * Otherwise, the row now labeled `appliedTargetLabel` becomes the new
   * selection (and the URL is rewritten to match); if no such row exists, the
   * selection is cleared. A reload failure leaves the selection untouched and
   * returns false. If the user changes or clears the selection while the
   * reload is in flight, that choice wins and the reselection is skipped.
   */
  async refreshAfterApply(originalKey: string, appliedTargetLabel: string): Promise<boolean> {
    const ok = await this.load({ background: true });
    if (!ok) return false;
    if (this.selectedProjectKey !== originalKey) return true;
    const rows = (this.inventory?.projects ?? []) as DbProjectInventoryRow[];
    if (rows.some((row) => row.project_key === originalKey)) return true;
    const target = rows.find((row) => row.label === appliedTargetLabel);
    this.selectedProjectKey = target ? target.project_key : "";
    this.writeUrl();
    return true;
  }
}

export const data = new DataStore();
