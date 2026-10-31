import { classifyHref, exportAtlasHtml, exportAtlasZip, exportBoardsHtml, exportBoardsZip, exportGlyphSource, exportSourcesZip,
  renderExportBoards, indexBoards, resolveFilePath, type AtlasDocument, type Diagram } from '@zoomflow/core';
import type { RenderBoard } from '@zoomflow/viewer';
import { prepareExportSvg, rasterizeSvg, offlineViewerScript, offlineViewerStyle } from '@zoomflow/viewer';
import type { DocumentExport, DocumentPane } from './document-pane';
import type { setupExamplePicker } from './example-picker';
import { describeError, plural, safeFilename } from './text';

/** The project paths of the files a diagram's file links open, in board order. */
function linkedFiles(diagram: Diagram, path: string): string[] {
  const paths: string[] = [];
  for (const board of indexBoards(diagram).values()) {
    for (const shape of board.diagram.shapes) {
      const link = classifyHref(shape.link ?? '');
      const target = link.kind === 'file' ? resolveFilePath(path, link.path) : undefined;
      if (target !== undefined && !paths.includes(target)) paths.push(target);
    }
  }
  return paths;
}

export function download(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = name;
  link.addEventListener('click', (event) => event.stopPropagation());
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/** The header Export menu: diagram exports and source downloads for the active pane. */
export function setupExportMenu(activePane: () => DocumentPane | undefined, picker: () => ReturnType<typeof setupExamplePicker>) {
  const moreToggle = document.querySelector<HTMLButtonElement>('#header-more-toggle')!;
  const moreContent = document.querySelector<HTMLElement>('#header-more-content')!;
  const more = document.querySelector<HTMLElement>('#header-more')!;
  function setMoreOpen(open: boolean) {
    moreContent.hidden = !open;
    moreToggle.setAttribute('aria-expanded', String(open));
  }
  moreToggle.addEventListener('click', () => { showName(); setMoreOpen(Boolean(moreContent.hidden)); });
  document.addEventListener('click', (event) => { if (event.target instanceof Node && !more.contains(event.target)) setMoreOpen(false); });
  more.addEventListener('focusout', (event) => { if (event.relatedTarget instanceof Node && !more.contains(event.relatedTarget)) setMoreOpen(false); });
  more.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && !moreContent.hidden) {
      event.preventDefault(); event.stopPropagation(); setMoreOpen(false); moreToggle.focus();
    }
  });

  // The name field shows the name without its extension; changing it renames the draft.
  const nameField = document.querySelector<HTMLInputElement>('#export-name')!;
  const nameHint = document.querySelector<HTMLElement>('#export-name-hint')!;
  function showName() {
    const pane = activePane();
    const project = Boolean(pane?.key && picker().projectName(pane.id));
    nameField.value = pane?.name.replace(/\.d2$/i, '') ?? '';
    nameField.disabled = !pane?.key || project;
    nameHint.textContent = project ? 'Project files keep their file name.' : 'Names the downloaded files.';
  }
  function commitName() {
    const pane = activePane();
    if (pane?.key) pane.rename(nameField.value);
    showName();
  }
  nameField.addEventListener('change', commitName);
  nameField.addEventListener('keydown', (event) => {
    // Enter commits the name and keeps focus in the open menu.
    if (event.key === 'Enter') { event.preventDefault(); commitName(); nameField.select(); }
    // Escape restores the name before the menu closes.
    else if (event.key === 'Escape') showName();
  });

  const exportStatus = document.querySelector<HTMLElement>('#export-status')!;
  const projectDownloadButton = document.querySelector<HTMLButtonElement>('#save-project')!;
  function setExportStatus(message: string, kind: 'info' | 'error' = 'info') {
    exportStatus.textContent = message;
    exportStatus.dataset.kind = kind;
    exportStatus.hidden = !message;
  }
  /** The export menu shows the result while open; the pane's live activity line announces it once. */
  function reportExport(message: string, kind: 'info' | 'error' = 'info', pane = activePane()!) {
    pane.setActivity(message, kind);
    setExportStatus(message, kind);
  }

  document.querySelector<HTMLButtonElement>('#save-source')!.addEventListener('click', () => {
    const pane = activePane()!;
    if (!pane.key) return;
    const name = safeFilename(pane.name.replace(/\.d2$/i, '')) + '.d2';
    download(new Blob([pane.source], { type: 'text/plain;charset=utf-8' }), name);
    picker().markDownloadRequested(pane.id);
    pane.setActivity(`Source download started: ${name}`);
  });
  projectDownloadButton.addEventListener('click', () => {
    const pane = activePane()!;
    const name = picker().projectName(pane.id);
    if (!name) return;
    try {
      const { fs, inputPath } = picker().compilation(pane.id);
      fs[inputPath] = pane.source;
      const filename = `${safeFilename(name)}.zip`;
      download(new Blob([exportSourcesZip(fs)], { type: 'application/zip' }), filename);
      reportExport(`Project source download started: ${filename}`);
    } catch (error) { reportExport(`Project source download failed: ${describeError(error)}`, 'error'); }
  });

  const exportButton = document.querySelector<HTMLButtonElement>('#export')!;
  const exportChoices = document.querySelector<HTMLFieldSetElement>('#export-format')!;
  const exportFormat = { get value() { return exportChoices.querySelector<HTMLInputElement>('input:checked')!.value; } };
  let explicitExportFormat = false;
  const atlasChoice = exportChoices.querySelector<HTMLInputElement>('input[value="atlas"]')!;
  /** Only project files offer the atlas; others fall back to one file's interactive HTML. */
  let wantsAtlas = false;
  const isProject = (pane: DocumentPane | undefined) => Boolean(pane?.key && picker().projectName(pane.id));
  try {
    const saved = localStorage.getItem('zoomflow.export-format');
    wantsAtlas = saved === 'atlas';
    const choice = [...exportChoices.querySelectorAll<HTMLInputElement>('input')].find(input => input.value === saved);
    if (choice) { choice.checked = true; explicitExportFormat = true; }
  } catch { /* Exporting works without stored preferences. */ }
  let exporting = false;
  const canExport = () => !exporting && Boolean(activePane()?.documentExport && activePane()!.current);
  function updateExportLabel() {
    const pane = activePane();
    if (document.activeElement !== nameField) showName();
    const documentExport = pane?.documentExport;
    exportButton.disabled = !canExport();
    const count = documentExport ? indexBoards(documentExport.diagram).size : 0;
    if (documentExport && !explicitExportFormat) {
      exportChoices.querySelector<HTMLInputElement>(`input[value="${count > 1 ? 'html' : 'svg'}"]`)!.checked = true;
    }
    const project = isProject(pane);
    atlasChoice.closest('label')!.hidden = !project;
    if (wantsAtlas) (project ? atlasChoice : exportChoices.querySelector<HTMLInputElement>('input[value="html"]')!).checked = true;
    const allBoards = exportFormat.value === 'zip' || exportFormat.value === 'html';
    const formatName = ({ svg: 'SVG', png: 'PNG', zip: 'SVG ZIP', html: 'Interactive HTML', atlas: 'Interactive atlas HTML' } as Record<string, string>)[exportFormat.value];
    document.querySelector('#export-scope')!.textContent = documentExport
      ? exportFormat.value === 'atlas' ? `This file and every file it links to · ${formatName}`
        : allBoards ? `Entire diagram${project && exportFormat.value === 'zip' ? ' and the files it links to' : ''} · ${plural(count, 'board')} · ${formatName}`
          : `Current board: ${pane!.viewer.currentBoard?.name ?? 'Root'} · ${formatName}`
      : pane?.sourceError ? 'Fix source errors to export.' : 'Compile the current source to export a diagram.';
    exportButton.textContent = ({ svg: 'Download SVG', png: 'Download PNG', zip: 'Download SVG ZIP',
      html: 'Download HTML', atlas: 'Download atlas HTML' } as Record<string, string>)[exportFormat.value] ?? 'Download export';
  }
  function rememberExportFormat() {
    explicitExportFormat = true;
    wantsAtlas = exportFormat.value === 'atlas';
    try { localStorage.setItem('zoomflow.export-format', exportFormat.value); } catch { /* Optional preference. */ }
    updateExportLabel();
  }
  exportChoices.addEventListener('change', rememberExportFormat);
  exportChoices.addEventListener('click', (event) => {
    // Clicking the automatically selected format is still an explicit choice.
    if (event.target instanceof HTMLInputElement && event.target.checked) rememberExportFormat();
  });

  exportButton.addEventListener('click', async () => {
    const pane = activePane()!;
    const loaded = pane.documentExport;
    const board = pane.viewer.currentBoard;
    if (!loaded || !board || !canExport()) return;
    const exportRevision = pane.generation;
    exporting = true;
    exportButton.disabled = true;
    const format = exportFormat.value;
    reportExport(`Preparing ${format === 'atlas' ? 'atlas' : format.toUpperCase()} download…`, 'info', pane);
    const dark = document.documentElement.dataset.theme === 'dark';
    const exportName = pane.name.replace(/\.d2$/i, '');
    const warnings: string[] = [];
    // Interactive HTML follows its reader's color scheme; images keep the theme shown here.
    const theme = format === 'html' || format === 'atlas' ? undefined : dark;
    const prepareWith = (render: RenderBoard) => async (target: string) => {
      const result = await prepareExportSvg(await render(target), theme);
      warnings.push(...result.warnings);
      return result.svg;
    };
    const prepare = prepareWith(loaded.render);
    const canceled = () => exportRevision !== pane.generation || pane.documentExport !== loaded;
    /** This file and every project file its file links reach, breadth first, each once. */
    async function atlasDocuments(): Promise<AtlasDocument[] | undefined> {
      const { fs, inputPath } = picker().compilation(pane.id);
      fs[inputPath] = pane.source;
      const queue = [inputPath];
      const documents: AtlasDocument[] = [];
      for (const [index, path] of queue.entries()) {
        reportExport(`Preparing ${format === 'atlas' ? 'atlas' : 'SVG ZIP'}: ${path} (${index + 1} of ${queue.length})…`, 'info', pane);
        let file: Pick<DocumentExport, 'diagram' | 'render' | 'diagnostics'> = loaded!;
        if (index) {
          try { file = await pane.compileProjectFile(path, fs); }
          catch (error) {
            // Links to a file that does not compile stay, marked missing.
            warnings.push(`Left out ${path}: ${describeError(error)}`);
            continue;
          }
        }
        if (canceled()) return undefined;
        const boards = await renderExportBoards(file.diagram, prepareWith(file.render), file.diagnostics);
        if (canceled()) return undefined;
        documents.push({ path, name: path.replace(/\.d2$/i, ''), boards });
        for (const linked of linkedFiles(file.diagram, path)) if (Object.hasOwn(fs, linked) && !queue.includes(linked)) queue.push(linked);
      }
      return documents;
    }
    try {
      let blob: Blob;
      let name = safeFilename(exportName);
      if (format === 'svg' || format === 'png') {
        const svg = await prepare(board.target);
        blob = format === 'png' ? await rasterizeSvg(svg) : new Blob([svg], { type: 'image/svg+xml' });
        name += `-${board.name === 'Root' ? 'root' : safeFilename(board.name)}.${format}`;
      } else if (format === 'atlas' || (format === 'zip' && isProject(pane))) {
        const documents = await atlasDocuments();
        if (!documents) {
          reportExport('Export canceled because the source changed.', 'info', pane);
          return;
        }
        if (format === 'zip') {
          blob = new Blob([exportAtlasZip(documents)], { type: 'application/zip' });
          name += '.zip';
        } else {
          const boards = documents.flatMap((document) => document.boards);
          const glyphSource = exportGlyphSource(boards);
          const glyphs = glyphSource ? await loaded.renderSource(glyphSource).catch(() => undefined) : undefined;
          blob = new Blob([exportAtlasHtml(documents, { title: exportName, script: offlineViewerScript(),
            style: offlineViewerStyle, glyphs })], { type: 'text/html' });
          name += '-atlas.html';
        }
      } else {
        const boards = await renderExportBoards(loaded.diagram, prepare, loaded.diagnostics);
        // Shared font subsets are an optimization; each board keeps its own fonts without them.
        const glyphSource = format === 'html' ? exportGlyphSource(boards) : undefined;
        const glyphs = glyphSource ? await loaded.renderSource(glyphSource).catch(() => undefined) : undefined;
        blob = format === 'zip' ? new Blob([exportBoardsZip(boards)], { type: 'application/zip' })
          : new Blob([exportBoardsHtml(boards, { title: exportName, script: offlineViewerScript(),
            style: offlineViewerStyle, glyphs })], { type: 'text/html' });
        name += `.${format}`;
      }
      if (canceled()) {
        reportExport('Export canceled because the source changed.', 'info', pane);
        return;
      }
      download(blob, name);
      const result = warnings.length ? `Download started: ${name}. ${[...new Set(warnings)].join(' ')}` : `Download started: ${name}`;
      reportExport(result, warnings.length > 0 ? 'error' : 'info', pane);
    } catch (error) {
      reportExport(`Export failed: ${describeError(error)}`, 'error', pane);
    }
    finally { exporting = false; updateExportLabel(); }
  });

  return {
    setExportStatus,
    updateExportLabel,
    /** Only project files offer the whole project as a download. */
    showProjectDownload(project: boolean) { projectDownloadButton.hidden = !project; },
  };
}
