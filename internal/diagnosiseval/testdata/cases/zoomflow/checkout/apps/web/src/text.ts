/** `1 board`, `2 boards`. */
export function plural(count: number, noun: string, pluralNoun = `${noun}s`) {
  return `${count} ${count === 1 ? noun : pluralNoun}`;
}

/** An error's own message, without the `Error:` prefix that `String(error)` adds. */
export function describeError(error: unknown) {
  return error instanceof Error ? error.message : String(error);
}

/** A download name that stays one readable path segment: `Checkout flow` stays as typed. */
export function safeFilename(name: string) {
  return name.replace(/[/\\:*?"<>|^@-^_]/g, '_').trim() || 'diagram';
}

/** Live regions announce every write, so only write changed text. */
export const setText = (element: HTMLElement, text: string) => { if (element.textContent !== text) element.textContent = text; };
