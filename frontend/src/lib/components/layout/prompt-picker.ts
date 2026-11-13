// The picker's button, so the `t` shortcut can open the picker from
// outside it.
let trigger: HTMLButtonElement | undefined;

export function setPromptPickerTrigger(button: HTMLButtonElement | undefined) {
  trigger = button;
}

/** Opens the prompt picker, as a click on its button does. */
export function openPromptPicker() {
  if (trigger?.getAttribute("aria-expanded") !== "true") trigger?.click();
}
