import { expect, test, type Page } from '@playwright/test';
import { readFile } from 'node:fs/promises';
import { readFileSync } from 'node:fs';
import { selectMenuOption, downloadExport, downloadSource, openDownloads } from './menu';
import { source, previewReady, stubAiStatus, chooseExample, projectFolder, openFolder } from './helpers';

/** Documents: drafts, recovery, saving to disk, downloads and removal with undo. */

async function draftRows(page: Page, name: string) {
  await page.locator('#example-picker > summary').click();
  const count = await page.locator('#draft-options .example-option').filter({ hasText: name }).count();
  await page.keyboard.press('Escape');
  return count;
}

test('draft recovers after reload and D2 source downloads', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('.zf-stage > svg')).toBeVisible();
  await source(page).fill('draft: Recovered');
  await expect(page.locator('#draft-status')).toHaveAttribute('data-recovery', 'saved');
  await page.reload();
  await expect(source(page)).toContainText('draft: Recovered');
  await expect(page.locator('.zf-stage')).toContainText('Recovered');
  const [download] = await Promise.all([
    page.waitForEvent('download'),
    downloadSource(page),
  ]);
  expect(download.suggestedFilename()).toBe('order.d2');
  expect(await readFile(await download.path(), 'utf8')).toBe('draft: Recovered');
  await expect(page.locator('#draft-status')).toContainText('Source download started');
  await page.reload();
  await expect(source(page)).toContainText('draft: Recovered');
  await expect(page.locator('#draft-status')).toHaveAttribute('data-recovery', 'saved');
  await page.locator('#file').setInputFiles({ name: 'custom.d2', mimeType: 'text/plain', buffer: Buffer.from('uploaded: Original') });
  await source(page).fill('uploaded: Edited');
  await page.reload();
  await expect(page.locator('#filename')).toHaveText('custom.d2');
  await expect(source(page)).toContainText('uploaded: Edited');
  await expect(page.locator('#draft-status')).toHaveAttribute('data-recovery', 'saved');
});

test('uploaded drafts can be removed without affecting other diagrams', async ({ page }) => {
  await page.goto('/');
  await page.locator('#file').setInputFiles({ name: 'old.d2', mimeType: 'text/plain', buffer: Buffer.from('old: Draft') });
  await page.locator('#file').setInputFiles({ name: 'active.d2', mimeType: 'text/plain', buffer: Buffer.from('active: Keep') });
  await page.locator('#example-picker summary').click();
  await page.getByRole('button', { name: 'Remove old.d2 draft' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Remove draft', exact: true }).click();
  await expect(page.locator('#filename')).toHaveText('active.d2');
  await expect(page.locator('.zf-stage')).toContainText('Keep');
  await page.locator('#example-picker summary').click();
  await page.getByRole('button', { name: 'Remove active.d2 draft' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Remove draft', exact: true }).click();
  await expect(page.locator('#filename')).toHaveText('order.d2');
  await page.reload();
  await page.locator('#example-picker summary').click();
  await expect(page.getByRole('button', { name: 'Remove old.d2 draft' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Remove active.d2 draft' })).toHaveCount(0);
});

test('file drops reject other formats and leave ordinary text drops alone', async ({ page }) => {
  await page.goto('/');
  const textPrevented = await page.evaluate(() => {
    const event = new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: new DataTransfer() });
    document.dispatchEvent(event);
    return event.defaultPrevented;
  });
  expect(textPrevented).toBe(false);
  await page.evaluate(() => {
    const transfer = new DataTransfer();
    transfer.items.add(new File(['not D2'], 'notes.txt', { type: 'text/plain' }));
    document.dispatchEvent(new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: transfer }));
  });
  await expect(page.locator('#activity-status')).toContainText('Choose a .d2 file');
  await expect(page.locator('#file-status')).toBeVisible();
  await expect(page.locator('#filename')).toHaveText('order.d2');
});

test('routine editing keeps status copy steady and exposes recovery details without layout movement', async ({ page }) => {
  await page.goto('/'); await previewReady(page);
  const before = await page.locator('#editor').boundingBox();
  await source(page).fill('a: First edit');
  await expect(page.locator('#draft-status')).toHaveText('File has unsaved changes');
  await expect(page.locator('#draft-status')).toHaveAttribute('data-recovery', 'saved');
  await expect(page.locator('#draft-status')).toHaveAttribute('title', /Browser draft saved/);
  await page.evaluate(() => {
    const el = document.querySelector('#draft-status')!;
    const updates: string[] = [];
    (window as typeof window & { statusUpdates: string[] }).statusUpdates = updates;
    new MutationObserver(() => updates.push(el.textContent!)).observe(el, { childList: true, subtree: true });
  });
  await source(page).press('End');
  await source(page).pressSequentially(' updated', { delay: 80 });
  await expect(page.locator('#draft-status')).toHaveAttribute('data-recovery', 'saved');
  await previewReady(page);
  expect(await page.evaluate(() => (window as typeof window & { statusUpdates: string[] }).statusUpdates)).toEqual([]);
  await expect(page.locator('#preview-status')).toBeHidden();
  await expect(page.locator('#lint-summary')).toBeHidden();
  expect((await page.locator('#editor').boundingBox())!.y).toBe(before!.y);
});

test('saving an example creates a named copy with its history, export names and reload identity', async ({ page }) => {
  await page.addInitScript(() => {
    const target = window as typeof window & { showSaveFilePicker?: () => Promise<unknown> };
    target.showSaveFilePicker = async () => ({ name: 'checkout-design.d2', createWritable: async () => ({
      write: async () => {}, close: async () => {}, abort: async () => {},
    }) });
  });
  await page.goto('/'); await previewReady(page);
  const original = await source(page).textContent();
  await source(page).fill('saved: After');
  await expect(page.locator('.zf-stage')).toContainText('After'); await previewReady(page);
  await source(page).press('ControlOrMeta+s');
  await expect(page.locator('#filename')).toHaveText('checkout-design.d2');
  await expect(page).toHaveTitle('checkout-design.d2 — Zoomflow');
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  await source(page).press('ControlOrMeta+z');
  await expect(source(page)).toHaveText(original!);
  await expect(page).toHaveTitle('• checkout-design.d2 — Zoomflow');
  await source(page).press('ControlOrMeta+Shift+z');
  await expect(source(page)).toHaveText('saved: After');
  await expect(page.locator('.zf-stage')).toContainText('After');
  await previewReady(page);
  await selectMenuOption(page, '#export-format', 'svg');
  const pending = page.waitForEvent('download');
  await downloadExport(page);
  expect((await pending).suggestedFilename()).toBe('checkout-design-root.svg');
  await page.locator('#example-picker summary').click();
  await expect(page.locator('#draft-options')).toContainText('Saved copy of Order flow');
  // The edits moved to the saved copy, so the example itself is no longer modified.
  await expect(page.locator('#example-options').getByRole('button', { name: /^Order flow/ }).locator('.example-modified')).toBeHidden();
  await expect(page.getByRole('button', { name: 'Restore original example: Order flow' })).toBeHidden();
  await page.keyboard.press('Escape');
  await page.reload(); await previewReady(page);
  await expect(page.locator('#filename')).toHaveText('checkout-design.d2');
  await expect(page).toHaveTitle('checkout-design.d2 — Zoomflow');
  await expect(source(page)).toHaveText('saved: After');
});

test('naming a diagram names its exports, keeps examples intact and makes Save ask for a new file', async ({ page }) => {
  await page.addInitScript(() => {
    const target = window as typeof window & { suggested?: string; showOpenFilePicker?: () => Promise<unknown[]>; showSaveFilePicker?: (options: { suggestedName: string }) => Promise<unknown> };
    const handle = (name: string) => ({ name, getFile: async () => new File(['a -> b'], name, { lastModified: 1 }),
      createWritable: async () => ({ write: async () => {}, close: async () => {}, abort: async () => {} }) });
    target.showOpenFilePicker = async () => [handle('work.d2')];
    target.showSaveFilePicker = async ({ suggestedName }) => { target.suggested = suggestedName; return handle(suggestedName); };
  });
  await page.goto('/'); await previewReady(page);
  await source(page).fill('named: Before');
  await expect(page.locator('.zf-stage')).toContainText('Before'); await previewReady(page);
  await openDownloads(page);
  await expect(page.locator('#export-name')).toHaveValue('order');
  await page.locator('#export-name').fill('Checkout plan');
  await page.locator('#export-name').press('Enter');
  await expect(page.locator('#filename')).toHaveText('Checkout plan.d2');
  await expect(page.locator('#export-name')).toHaveValue('Checkout plan');
  // Enter commits the name and keeps focus in the open menu.
  await expect(page.locator('#export-name')).toBeFocused();
  await expect(page.locator('#draft-status')).toHaveText('File has unsaved changes');
  await selectMenuOption(page, '#export-format', 'svg');
  const pending = page.waitForEvent('download');
  await downloadExport(page);
  expect((await pending).suggestedFilename()).toBe('Checkout plan-root.svg');
  // The edits moved to the named draft with their history; the example is unedited.
  await source(page).press('ControlOrMeta+z');
  await expect(source(page)).not.toContainText('named: Before');
  await source(page).press('ControlOrMeta+Shift+z');
  await page.locator('#example-picker summary').click();
  await expect(page.locator('#draft-options')).toContainText('Saved copy of Order flow');
  await expect(page.locator('#example-options').getByRole('button', { name: /^Order flow/ }).locator('.example-modified')).toBeHidden();
  await page.keyboard.press('Escape');
  await page.reload(); await previewReady(page);
  await expect(page.locator('#filename')).toHaveText('Checkout plan.d2');
  await expect(source(page)).toHaveText('named: Before');

  // A file opened from disk keeps its name there, so Save asks where to save under the new one.
  await page.locator('#file').click();
  await expect(page.locator('#filename')).toHaveText('work.d2'); await previewReady(page);
  await openDownloads(page);
  await page.locator('#export-name').fill('renamed work');
  await page.locator('#export-name').press('Enter');
  await expect(page.locator('#filename')).toHaveText('renamed work.d2');
  await source(page).press('ControlOrMeta+s');
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  expect(await page.evaluate(() => (window as typeof window & { suggested?: string }).suggested)).toBe('renamed work.d2');
});

test('the save shortcut applies a name still being typed before saving', async ({ page }) => {
  await page.addInitScript(() => {
    const target = window as typeof window & { suggested?: string; showOpenFilePicker?: () => Promise<unknown[]>; showSaveFilePicker?: (options: { suggestedName: string }) => Promise<unknown> };
    const handle = (name: string) => ({ name, getFile: async () => new File(['a -> b'], name, { lastModified: 1 }),
      createWritable: async () => ({ write: async () => {}, close: async () => {}, abort: async () => {} }) });
    target.showOpenFilePicker = async () => [handle('work.d2')];
    target.showSaveFilePicker = async ({ suggestedName }) => { target.suggested = suggestedName; return handle(suggestedName); };
  });
  await page.goto('/'); await previewReady(page);
  await page.locator('#file').click();
  await expect(page.locator('#filename')).toHaveText('work.d2'); await previewReady(page);
  await openDownloads(page);
  await page.locator('#export-name').fill('typed name');
  await page.locator('#export-name').press('ControlOrMeta+s');
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  expect(await page.evaluate(() => (window as typeof window & { suggested?: string }).suggested)).toBe('typed name.d2');
  await expect(page.locator('#filename')).toHaveText('typed name.d2');
  // Leaving the field afterwards keeps the file the save chose.
  await page.locator('#export-name').blur();
  await expect(page.locator('#activity-status')).not.toContainText('Save asks where to save it');
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
});

test('a blank or unedited name keeps an opened file linked to it', async ({ page }) => {
  await page.addInitScript(() => {
    const target = window as typeof window & { written?: string[]; showOpenFilePicker?: () => Promise<unknown[]>; showSaveFilePicker?: () => Promise<unknown> };
    target.written = [];
    const handle = { name: 'Work.D2', getFile: async () => new File(['a -> b'], 'Work.D2', { lastModified: 1 }),
      createWritable: async () => ({ write: async (text: string) => { target.written!.push(text); }, close: async () => {}, abort: async () => {} }) };
    target.showOpenFilePicker = async () => [handle];
    target.showSaveFilePicker = async () => { throw new Error('Save should write back to Work.D2'); };
  });
  await page.goto('/'); await previewReady(page);
  await page.locator('#file').click();
  await expect(page.locator('#filename')).toHaveText('Work.D2'); await previewReady(page);
  await openDownloads(page);
  await expect(page.locator('#export-name')).toHaveValue('Work');
  await page.locator('#export-name').fill('  ');
  await page.locator('#export-name').press('Enter');
  await expect(page.locator('#export-name')).toHaveValue('Work');
  // Saving from the unedited field must not rename `Work.D2` to `Work.d2` and unlink it.
  await page.locator('#export-name').press('ControlOrMeta+s');
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  await expect(page.locator('#filename')).toHaveText('Work.D2');
  expect(await page.evaluate(() => (window as typeof window & { written?: string[] }).written)).toEqual(['a -> b']);
});

test('a slow save preserves later edits and does not rename a document opened in the meantime', async ({ page }) => {
  await page.addInitScript(() => {
    (window as any).showSaveFilePicker = async () => ({ name: 'saved-copy.d2', createWritable: async () => ({
      write: async (text: string) => { (window as any).written = text; await new Promise<void>((resolve) => { (window as any).finishSave = resolve; }); },
      close: async () => {}, abort: async () => {},
    }) });
  });
  await page.goto('/'); await previewReady(page);
  await page.locator('#file').setInputFiles({ name: 'original.d2', mimeType: 'text/plain', buffer: Buffer.from('a: Original') });
  await expect(page.locator('#filename')).toHaveText('original.d2'); await previewReady(page);
  await source(page).fill('a: Saved revision');
  await source(page).press('ControlOrMeta+s');
  await expect.poll(() => page.evaluate(() => typeof (window as any).finishSave)).toBe('function');
  await expect(page.locator('#activity-status')).toHaveText('Saving file…');
  await source(page).fill('a: Later edit');
  await page.locator('#file').setInputFiles({ name: 'unrelated.d2', mimeType: 'text/plain', buffer: Buffer.from('b: Unrelated') });
  await expect(page.locator('#filename')).toHaveText('unrelated.d2');
  await page.evaluate(() => (window as any).finishSave());
  await expect(page.locator('#save-file')).toBeEnabled();
  await expect(page).toHaveTitle('unrelated.d2 — Zoomflow');
  await page.locator('#example-picker summary').click();
  await page.locator('#draft-options .example-option').filter({ hasText: 'saved-copy.d2' }).click();
  await expect(source(page)).toHaveText('a: Later edit');
  await expect(page).toHaveTitle('• saved-copy.d2 — Zoomflow');
  await expect(page.locator('#draft-status')).toHaveText('File has unsaved changes');
  expect(await page.evaluate(() => (window as any).written)).toBe('a: Saved revision');
  await page.locator('#example-picker summary').click();
  await expect(page.locator('#draft-options .example-option')).toHaveCount(2);
  await expect(page.getByRole('button', { name: 'Remove saved-copy.d2 draft', exact: true })).toBeVisible();
});

test('canceling a slow file save clears progress and retains the original document identity', async ({ page }) => {
  await page.addInitScript(() => {
    (window as any).showSaveFilePicker = async () => ({ name: 'canceled.d2', createWritable: async () => ({
      write: async () => new Promise((_resolve, reject) => { (window as any).cancelSave = () => reject(new DOMException('Canceled', 'AbortError')); }),
      close: async () => {}, abort: async () => {},
    }) });
  });
  await page.goto('/'); await previewReady(page);
  await source(page).fill('a: Keep this');
  await source(page).press('ControlOrMeta+s');
  await expect(page.locator('#activity-status')).toHaveText('Saving file…');
  await page.evaluate(() => (window as any).cancelSave());
  await expect(page.locator('#activity-status')).toBeHidden();
  await expect(page.locator('#save-file')).toBeEnabled();
  await expect(page.locator('#filename')).toHaveText('order.d2');
  await expect(source(page)).toHaveText('a: Keep this');
  await expect(page).toHaveTitle('• order.d2 — Zoomflow');
});

test('same-name documents keep separate undo histories and drafts', async ({ page }) => {
  await page.goto('/');
  await previewReady(page);
  await page.locator('#file').setInputFiles({ name: 'same.d2', mimeType: 'text/plain', buffer: Buffer.from('a: Original A') });
  await previewReady(page);
  await source(page).fill('a: Edited A');
  await previewReady(page);
  await page.locator('#file').setInputFiles({ name: 'same.d2', mimeType: 'text/plain', buffer: Buffer.from('b: Original B') });
  await previewReady(page);
  await source(page).press('ControlOrMeta+z');
  await expect(source(page)).toHaveText('b: Original B');
  await source(page).fill('b: Edited B');
  await previewReady(page);
  await chooseExample(page, /same\.d2.*Draft 1/);
  await source(page).press('ControlOrMeta+z');
  await expect(source(page)).toHaveText('a: Original A');
  await chooseExample(page, /same\.d2.*Draft 2/);
  await source(page).press('ControlOrMeta+z');
  await expect(source(page)).toHaveText('b: Original B');
});

test('header stays stable while saving and preferences survive reload', async ({ page }) => {
  await page.setViewportSize({ width: 1024, height: 768 });
  await page.goto('/');
  await previewReady(page);
  const before = await page.locator('header').boundingBox();
  const title = (await page.locator('header > strong').boundingBox())!;
  const open = (await page.locator('.file-button').boundingBox())!;
  const picker = (await page.locator('#example-picker summary').boundingBox())!;
  const center = (box: { y: number; height: number }) => box.y + box.height / 2;
  expect(Math.abs(center(title) - center(picker))).toBeLessThan(1);
  expect(Math.abs(center(open) - center(picker))).toBeLessThan(1);
  const status = (await page.locator('#draft-status').boundingBox())!;
  const save = (await page.locator('#save-file').boundingBox())!;
  expect(status.y).toBeGreaterThanOrEqual(save.y + save.height);
  const statusRow = (await page.locator('#work-status').boundingBox())!;
  expect(statusRow.height).toBeLessThanOrEqual(40);
  expect(status.y).toBeGreaterThanOrEqual(statusRow.y);
  expect(status.y + status.height).toBeLessThanOrEqual(statusRow.y + statusRow.height);
  expect(statusRow.y + statusRow.height).toBeLessThanOrEqual((await page.locator('#editor').boundingBox())!.y);
  await source(page).fill('a: Edited');
  expect(await page.locator('header').boundingBox()).toEqual(before);
  await expect(page.locator('#draft-status')).toHaveAttribute('data-recovery', 'saved');
  expect(await page.locator('header').boundingBox()).toEqual(before);
  await selectMenuOption(page, '#theme', 'dark');
  await selectMenuOption(page, '#export-format', 'html');
  await page.reload();
  await previewReady(page);
  await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
  await openDownloads(page);
  await expect(page.locator('#export-format input[value="html"]')).toBeChecked();
  await page.locator('#export-format input[value="html"]').press('Escape');
  await expect(page.locator('#header-more-toggle')).toBeFocused();
  await expect(page.locator('#header-more-content')).toBeHidden();
});

test('removed file undo restores its save handle, board view and editor history', async ({ page }) => {
  await stubAiStatus(page);
  const text = readFileSync('fixtures/order.d2', 'utf8');
  await page.addInitScript(text => {
    const target = window as typeof window & { written?: string; showOpenFilePicker?: () => Promise<unknown[]> };
    target.showOpenFilePicker = async () => [{ name: 'work.d2', getFile: async () => new File([text], 'work.d2', { lastModified: 1 }),
      createWritable: async () => ({ write: async (value: string) => { target.written = value; }, close: async () => {}, abort: async () => {} }) }];
  }, text);
  await page.goto('/'); await previewReady(page);
  await page.locator('#file').click();
  await expect(page.locator('#filename')).toHaveText('work.d2'); await previewReady(page);
  await source(page).fill(`${text}\n# Edited`); await previewReady(page);
  await page.locator('.zf-stage a[data-board="root.layers.checkout"]').click();
  await expect(page.locator('.zf-stage')).toHaveAttribute('data-board', 'root.layers.checkout');
  await page.getByRole('button', { name: 'Zoom in', exact: true }).click();
  const box = await page.locator('.zf-stage > svg').getAttribute('viewBox');
  await page.locator('#example-picker summary').click();
  await page.getByRole('button', { name: 'Remove work.d2 draft' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Remove draft', exact: true }).click();
  await expect(page.locator('#filename')).toHaveText('order.d2'); await previewReady(page);
  await page.locator('#document-undo').getByRole('button', { name: 'Undo', exact: true }).click();
  await expect(page.locator('#filename')).toHaveText('work.d2'); await previewReady(page);
  await expect(page.locator('.zf-stage')).toHaveAttribute('data-board', 'root.layers.checkout');
  await expect(page.locator('.zf-stage > svg')).toHaveAttribute('viewBox', box!);
  await source(page).press('ControlOrMeta+s');
  await expect.poll(() => page.evaluate(() => (window as typeof window & { written?: string }).written)).toBe(`${text}\n# Edited`);
  await source(page).press('ControlOrMeta+z');
  await expect(source(page)).not.toContainText('# Edited');
});

test('undo notices pause while focused and expire without restoring removed drafts', async ({ page }) => {
  await stubAiStatus(page);
  await page.goto('/'); await previewReady(page);
  await page.clock.install();
  await page.locator('#file').setInputFiles({ name: 'temporary.d2', mimeType: 'text/plain', buffer: Buffer.from('a: Temporary') });
  await previewReady(page);
  await page.locator('#example-picker summary').click();
  await page.getByRole('button', { name: 'Remove temporary.d2 draft' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Remove draft', exact: true }).click();
  const notice = page.locator('#document-undo');
  await notice.getByRole('button', { name: 'Undo', exact: true }).focus();
  await page.clock.fastForward(31_000);
  await expect(notice).toBeVisible();
  await source(page).focus();
  await page.clock.fastForward(31_000);
  await expect(notice).toBeHidden();
  await page.reload(); await previewReady(page);
  await page.locator('#example-picker summary').click();
  await expect(page.locator('.example-option').filter({ hasText: 'temporary.d2' })).toHaveCount(0);
});

test('unsaved marker survives activity, errors and downloads, and follows disk save and undo', async ({ page }) => {
  await stubAiStatus(page);
  await page.addInitScript(() => {
    (window as any).showSaveFilePicker = async () => ({ name: 'kept.d2', createWritable: async () => ({
      write: async () => {}, close: async () => {}, abort: async () => {},
    }) });
  });
  await page.goto('/'); await previewReady(page);
  const marker = page.locator('#document-unsaved');
  await expect(marker).toBeHidden();
  await source(page).fill('a:{label:Changed}');
  await expect(marker).toBeVisible();
  await previewReady(page);
  await page.locator('#format-source').click();
  await expect(page.locator('#format-status')).toBeVisible();
  await expect(marker).toBeVisible();
  await previewReady(page);
  await selectMenuOption(page, '#export-format', 'svg');
  const download = page.waitForEvent('download');
  await page.locator('#export').click(); await download;
  await expect(page.locator('#activity-status')).toContainText('Download started');
  await expect(marker).toBeVisible();
  await page.locator('#header-more-toggle').click();
  await source(page).fill('a: {');
  await expect(page.locator('#compile-error')).toBeVisible();
  await expect(marker).toBeVisible();
  await source(page).fill('a: Saved'); await previewReady(page);
  await page.locator('#save-file').click();
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  await expect(marker).toBeHidden();
  await source(page).press('ControlOrMeta+z');
  await expect(marker).toBeVisible();
});

test('save shortcut writes to the chosen file and reports disk state beside Save', async ({ page }) => {
  await page.addInitScript(() => {
    (window as typeof window & { savedSource?: string; showSaveFilePicker?: () => Promise<unknown> }).showSaveFilePicker = async () => ({
      name: 'saved.d2', createWritable: async () => ({
        write: async (text: string) => { (window as typeof window & { savedSource?: string }).savedSource = text; },
        close: async () => {}, abort: async () => {},
      }),
    });
  });
  await page.goto('/');
  await source(page).fill('saved: Shortcut');
  await source(page).press('ControlOrMeta+s');
  await expect(page.locator('#draft-status')).toContainText('Saved to file');
  expect(await page.evaluate(() => (window as typeof window & { savedSource?: string }).savedSource)).toBe('saved: Shortcut');
  await source(page).fill('saved: Changed again');
  await expect(page.locator('#draft-status')).toContainText('File has unsaved changes');
  await expect(page.locator('#work-status #draft-status')).toBeVisible();
});

test('download fallback is explicit and never claims a disk save or unavailable recovery', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(window, 'showSaveFilePicker', { value: undefined, configurable: true });
    Storage.prototype.setItem = () => { throw new Error('Storage blocked'); };
  });
  await page.goto('/');
  await expect(page.locator('#save-file')).toHaveText('Download .d2');
  await source(page).fill('download: Keep this');
  const [download] = await Promise.all([page.waitForEvent('download'), source(page).press('ControlOrMeta+s')]);
  expect(await readFile(await download.path(), 'utf8')).toBe('download: Keep this');
  await expect(page.locator('#draft-status')).toContainText('Source download started');
  await expect(page.locator('#draft-status')).toContainText('Browser draft unavailable');
  await expect(page.locator('#draft-status')).not.toContainText('Saved to file');
});

test('a page restored from the back/forward cache keeps editing and previewing', async ({ page }) => {
  await page.goto('/'); await previewReady(page);
  await page.evaluate(() => {
    window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }));
    window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }));
  });
  await source(page).fill('back: Returned from history');
  await previewReady(page);
  await expect(page.locator('.zf-stage')).toContainText('Returned from history');
});

test('hiding the page saves the latest edit without waiting for the autosave', async ({ page }) => {
  await page.goto('/'); await previewReady(page);
  await page.clock.install();
  await source(page).fill('hidden: Saved on hide');
  const saved = await page.evaluate(() => {
    const stored = () => Object.keys(localStorage).some((key) => key.startsWith('zoomflow.draft.v2:') && localStorage.getItem(key)!.includes('Saved on hide'));
    const before = stored();
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
    return [before, stored()];
  });
  expect(saved).toEqual([false, true]);
});

test('reopening an unchanged file, folder or empty draft reuses its draft', async ({ page }) => {
  await page.goto('/'); await previewReady(page);
  const file = { name: 'repeat.d2', mimeType: 'text/plain', buffer: Buffer.from('repeat: Same file') };
  await page.locator('#file').setInputFiles(file); await previewReady(page);
  await page.locator('#file').setInputFiles(file); await previewReady(page);
  expect(await draftRows(page, 'repeat.d2')).toBe(1);
  // A draft with unsaved changes is kept; the file opens as a new draft.
  await source(page).fill('repeat: Edited');
  await previewReady(page);
  await page.locator('#file').setInputFiles(file); await previewReady(page);
  await expect(source(page)).toHaveText('repeat: Same file');
  expect(await draftRows(page, 'repeat.d2')).toBe(2);

  await openFolder(page, projectFolder); await previewReady(page);
  await openFolder(page, projectFolder); await previewReady(page);
  await page.locator('#example-picker > summary').click();
  await expect(page.locator('.project-group')).toHaveCount(1);
  await page.keyboard.press('Escape');

  await page.setViewportSize({ width: 1600, height: 900 });
  await page.selectOption('#layout', 'tabs');
  await page.locator('#pane-start-empty-b').click();
  await previewReady(page, 'b');
  await page.selectOption('#layout', 'single');
  await page.selectOption('#layout', 'tabs');
  await page.locator('#pane-start-empty-b').click();
  await previewReady(page, 'b');
  expect(await draftRows(page, 'untitled.d2')).toBe(1);
});

test('reopening a picked file matches its file handle', async ({ page }) => {
  await page.addInitScript(() => {
    const disk: Record<string, string> = { first: 'picked: First version', other: 'picked: First version' };
    const handle = (id: string) => ({ id, kind: 'file', name: 'picked.d2',
      getFile: async () => new File([disk[id]], 'picked.d2', { type: 'text/plain' }),
      isSameEntry: async (other: { id?: string }) => other.id === id });
    (window as any).disk = disk;
    (window as any).nextHandle = 'first';
    (window as any).showOpenFilePicker = async () => [handle((window as any).nextHandle)];
  });
  await page.goto('/'); await previewReady(page);
  await page.locator('label.file-button').click(); await previewReady(page);
  await expect(page.locator('.zf-stage')).toContainText('First version');
  // The same file changed on disk: its unchanged draft is reused with the new contents.
  await page.evaluate(() => { (window as any).disk.first = 'picked: Second version'; });
  await page.locator('label.file-button').click();
  await expect(page.locator('.zf-stage')).toContainText('Second version'); await previewReady(page);
  expect(await draftRows(page, 'picked.d2')).toBe(1);
  // Another file with the same name and contents is a separate draft.
  await page.evaluate(() => { (window as any).nextHandle = 'other'; });
  await page.locator('label.file-button').click();
  await expect(page.locator('.zf-stage')).toContainText('First version'); await previewReady(page);
  expect(await draftRows(page, 'picked.d2')).toBe(2);
});

test('saving asks before overwriting a file that changed on disk since it was opened or saved', async ({ page }) => {
  await page.addInitScript(() => {
    const disk = { text: 'disk: Original', modified: 1 };
    (window as any).disk = disk;
    (window as any).showOpenFilePicker = async () => [{ kind: 'file', name: 'shared.d2',
      getFile: async () => new File([disk.text], 'shared.d2', { type: 'text/plain', lastModified: disk.modified }),
      createWritable: async () => ({ write: async (text: string) => { disk.text = text; disk.modified++; }, close: async () => {}, abort: async () => {} }) }];
  });
  await page.goto('/'); await previewReady(page);
  await page.locator('#file').click();
  await expect(page.locator('#filename')).toHaveText('shared.d2'); await previewReady(page);
  await source(page).fill('disk: First save');
  await page.locator('#save-file').click();
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  // Saving again after our own write does not ask.
  await source(page).fill('disk: Second save');
  await page.locator('#save-file').click();
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  expect(await page.evaluate(() => (window as any).disk.text)).toBe('disk: Second save');
  // Another program changes the file.
  await page.evaluate(() => { (window as any).disk.text = 'disk: Edited elsewhere'; (window as any).disk.modified = 99; });
  await source(page).fill('disk: Mine');
  await page.locator('#save-file').click();
  const dialog = page.getByRole('dialog', { name: 'File changed on disk — overwrite?' });
  await expect(dialog).toContainText('shared.d2 changed outside Zoomflow');
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click();
  await expect(page.locator('#activity-status')).toHaveText('Save canceled. The file on disk is unchanged.');
  expect(await page.evaluate(() => (window as any).disk.text)).toBe('disk: Edited elsewhere');
  await expect(page.locator('#draft-status')).toHaveText('File has unsaved changes');
  await page.locator('#save-file').click();
  await dialog.getByRole('button', { name: 'Overwrite file', exact: true }).click();
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  expect(await page.evaluate(() => (window as any).disk.text)).toBe('disk: Mine');
});

test('saving a file that was moved or deleted on disk asks where to save it', async ({ page }) => {
  await page.addInitScript(() => {
    const state = { deleted: false, opened: '', saved: '', pickerCalls: 0, cancelPicker: true };
    (window as any).state = state;
    (window as any).showOpenFilePicker = async () => [{ kind: 'file', name: 'gone.d2',
      getFile: async () => {
        if (state.deleted) throw new DOMException('A requested file or directory could not be found.', 'NotFoundError');
        return new File(['gone: Original'], 'gone.d2', { type: 'text/plain', lastModified: 1 });
      },
      createWritable: async () => {
        if (state.deleted) throw new DOMException('A requested file or directory could not be found.', 'NotFoundError');
        return { write: async (text: string) => { state.opened = text; }, close: async () => {}, abort: async () => {} };
      } }];
    (window as any).showSaveFilePicker = async () => {
      state.pickerCalls++;
      if (state.cancelPicker) throw new DOMException('Canceled', 'AbortError');
      return { name: 'moved.d2', getFile: async () => new File([state.saved], 'moved.d2', { lastModified: 2 }),
        createWritable: async () => ({ write: async (text: string) => { state.saved = text; }, close: async () => {}, abort: async () => {} }) };
    };
  });
  await page.goto('/'); await previewReady(page);
  await page.locator('#file').click();
  await expect(page.locator('#filename')).toHaveText('gone.d2'); await previewReady(page);
  await page.evaluate(() => { (window as any).state.deleted = true; });
  await source(page).fill('gone: Mine');
  await page.locator('#save-file').click();
  await expect(page.locator('#activity-status')).toHaveText('The file was moved or deleted; choose where to save.');
  await expect(page.locator('#draft-status')).toHaveText('File has unsaved changes');
  await page.evaluate(() => { (window as any).state.cancelPicker = false; });
  await page.locator('#save-file').click();
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  await expect(page.locator('#activity-status')).not.toContainText('moved or deleted');
  // Later saves go to the newly chosen file without asking again.
  await source(page).fill('gone: Again');
  await page.locator('#save-file').click();
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  expect(await page.evaluate(() => (window as any).state)).toMatchObject({ opened: '', saved: 'gone: Again', pickerCalls: 2 });
});

test('Save writes back to an opened file after reload and still asks about outside changes', async ({ page, browserName }) => {
  // A real handle from the origin-private file system can be kept in IndexedDB across reloads.
  test.skip(browserName !== 'chromium', 'Writable origin-private file handles');
  await page.addInitScript(() => {
    const w = window as any;
    w.pickerCalls = 0;
    w.showSaveFilePicker = async () => { w.pickerCalls++; throw new DOMException('Canceled', 'AbortError'); };
    w.showOpenFilePicker = async () => [await (await navigator.storage.getDirectory()).getFileHandle('kept.d2')];
    w.writeDisk = async (text: string) => {
      const writable = await (await (await navigator.storage.getDirectory()).getFileHandle('kept.d2', { create: true })).createWritable();
      await writable.write(text); await writable.close();
    };
    w.readDisk = async () => (await (await (await navigator.storage.getDirectory()).getFileHandle('kept.d2')).getFile()).text();
  });
  await page.goto('/'); await previewReady(page);
  await page.evaluate(() => (window as any).writeDisk('kept: Original'));
  await page.locator('#file').click();
  await expect(page.locator('#filename')).toHaveText('kept.d2'); await previewReady(page);
  await page.reload(); await previewReady(page);
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  await source(page).fill('kept: After reload');
  await page.locator('#save-file').click();
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  expect(await page.evaluate(() => (window as any).readDisk())).toBe('kept: After reload');
  // Another program changes the file while the page is closed.
  await page.waitForTimeout(20);
  await page.evaluate(() => (window as any).writeDisk('kept: Edited elsewhere'));
  await page.reload(); await previewReady(page);
  await source(page).fill('kept: Mine');
  await page.locator('#save-file').click();
  const dialog = page.getByRole('dialog', { name: 'File changed on disk — overwrite?' });
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click();
  expect(await page.evaluate(() => (window as any).readDisk())).toBe('kept: Edited elsewhere');
  expect(await page.evaluate(() => (window as any).pickerCalls)).toBe(0);
});

test('after a reload, Open and Save wait for stored file handles, and handles of lost drafts are removed', async ({ page, browserName }) => {
  test.skip(browserName !== 'chromium', 'Writable origin-private file handles');
  await page.addInitScript(() => {
    const w = window as any;
    w.pickerCalls = 0;
    w.showSaveFilePicker = async () => { w.pickerCalls++; throw new DOMException('Canceled', 'AbortError'); };
    w.showOpenFilePicker = async () => [await (await navigator.storage.getDirectory()).getFileHandle('kept.d2', { create: true })];
    w.storedKeys = () => new Promise<IDBValidKey[]>((resolve) => {
      const request = indexedDB.open('zoomflow.files');
      request.onsuccess = () => {
        const keys = request.result.transaction('handles', 'readonly').objectStore('handles').getAllKeys();
        keys.onsuccess = () => { resolve(keys.result); request.result.close(); };
      };
    });
    // Stored handles load slowly after the reload, as they may with a busy disk.
    if (sessionStorage.getItem('slow')) {
      const getAll = IDBObjectStore.prototype.getAll;
      IDBObjectStore.prototype.getAll = function (...args) {
        const request = getAll.apply(this, args);
        let handler: ((event: Event) => void) | null = null;
        Object.defineProperty(request, 'onsuccess', { get: () => handler, set: (value) => { handler = value; } });
        request.addEventListener('success', (event) => setTimeout(() => handler?.call(request, event), 1500));
        return request;
      };
    }
  });
  await page.goto('/'); await previewReady(page);
  await page.locator('#file').click();
  await expect(page.locator('#filename')).toHaveText('kept.d2'); await previewReady(page);
  // A handle whose draft is gone, as after clearing site data.
  await page.evaluate(async () => {
    const handle = await (await navigator.storage.getDirectory()).getFileHandle('ghost.d2', { create: true });
    await new Promise<void>((resolve) => {
      const request = indexedDB.open('zoomflow.files');
      request.onsuccess = () => {
        const transaction = request.result.transaction('handles', 'readwrite');
        transaction.objectStore('handles').put({ handle }, 'file:ghost');
        transaction.oncomplete = () => { request.result.close(); resolve(); };
      };
    });
    sessionStorage.setItem('slow', '1');
  });
  await page.reload();
  // Saving at once writes back rather than asking where to save.
  await source(page).fill('kept: Saved at once');
  await page.locator('#save-file').click();
  await expect(page.locator('#draft-status')).toHaveText('Saved to file');
  expect(await page.evaluate(() => (window as any).pickerCalls)).toBe(0);
  await expect.poll(() => page.evaluate(() => (window as any).storedKeys())).not.toContain('file:ghost');
  // Another program changes the file; opening it at once returns to its draft rather than adding another.
  await page.evaluate(async () => {
    const writable = await (await (await navigator.storage.getDirectory()).getFileHandle('kept.d2')).createWritable();
    await writable.write('kept: Edited elsewhere'); await writable.close();
  });
  await page.reload();
  await page.locator('#file').click();
  await expect(page.locator('#filename')).toHaveText('kept.d2'); await previewReady(page);
  expect(await draftRows(page, 'kept.d2')).toBe(1);
});

test('refusing to let Zoomflow write a file asks where to save, once', async ({ page, browserName }) => {
  test.skip(browserName !== 'chromium', 'Writable origin-private file handles');
  await page.addInitScript(() => {
    const w = window as any;
    w.permissionPrompts = 0;
    w.saveDialogs = 0;
    w.showOpenFilePicker = async () => [await (await navigator.storage.getDirectory()).getFileHandle('locked.d2', { create: true })];
    const prototype = FileSystemHandle.prototype as any;
    prototype.queryPermission = async () => 'prompt';
    prototype.requestPermission = async () => { w.permissionPrompts++; return 'denied'; };
    // Answering the permission prompt used up the click that would open the save dialog.
    w.showSaveFilePicker = async () => {
      w.saveDialogs++;
      if (w.saveDialogs === 1) throw new DOMException('Must be handling a user gesture', 'SecurityError');
      throw new DOMException('Canceled', 'AbortError');
    };
  });
  await page.goto('/'); await previewReady(page);
  await page.locator('#file').click();
  await expect(page.locator('#filename')).toHaveText('locked.d2'); await previewReady(page);
  await source(page).fill('locked: Mine');
  await page.locator('#save-file').click();
  await expect(page.locator('#activity-status')).toHaveText('Click Save again to choose where to save.');
  await page.locator('#save-file').click();
  await expect.poll(() => page.evaluate(() => (window as any).saveDialogs)).toBe(2);
  expect(await page.evaluate(() => (window as any).permissionPrompts)).toBe(1);
});

test('leaving the page asks first when edits exist only in memory because browser backup failed', async ({ page }) => {
  await page.addInitScript(() => { Storage.prototype.setItem = () => { throw new Error('Storage blocked'); }; });
  await page.goto('/'); await previewReady(page);
  const prompts = () => page.evaluate(() => {
    const event = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
  });
  expect(await prompts()).toBe(false);
  await source(page).fill('memory: Only here');
  await expect(page.locator('#draft-status')).toContainText('Browser draft unavailable');
  expect(await prompts()).toBe(true);
  await source(page).press('ControlOrMeta+z');
  await expect(page.locator('#document-unsaved')).toBeHidden();
  expect(await prompts()).toBe(false);
});

test('dropping several files opens the first and says to drop one at a time', async ({ page }) => {
  await page.goto('/'); await previewReady(page);
  await page.evaluate(() => {
    const transfer = new DataTransfer();
    transfer.items.add(new File(['first: First'], 'first.d2', { type: 'text/plain' }));
    transfer.items.add(new File(['second: Second'], 'second.d2', { type: 'text/plain' }));
    document.dispatchEvent(new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: transfer }));
  });
  await expect(page.locator('#filename')).toHaveText('first.d2');
  await expect(page.locator('#activity-status')).toHaveText('Opened first.d2; drop one file at a time.');
  expect(await draftRows(page, 'second.d2')).toBe(0);
});
