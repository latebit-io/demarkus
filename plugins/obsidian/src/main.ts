import { App, Notice, Plugin, Modal, Setting, TFile } from "obsidian";
import {
  DemarkusSettings,
  DEFAULT_SETTINGS,
  DemarkusSettingTab,
} from "./settings";
import * as cli from "./cli";
import {
  getMarkFrontmatter,
  buildFrontmatter,
  stripExistingFrontmatter,
} from "./frontmatter";

export default class DemarkusPlugin extends Plugin {
  settings: DemarkusSettings = DEFAULT_SETTINGS;

  async onload() {
    await this.loadSettings();

    this.addCommand({
      id: "fetch-document",
      name: "Fetch document",
      callback: () => this.fetchDocument(),
    });

    this.addCommand({
      id: "publish-note",
      name: "Publish note",
      editorCallback: () => this.publishNote(),
    });

    this.addCommand({
      id: "list-documents",
      name: "List documents",
      callback: () => this.listDocuments(),
    });

    this.addSettingTab(new DemarkusSettingTab(this.app, this));
  }

  private cliOpts(): cli.CliOptions {
    return {
      cliPath: this.settings.cliPath,
      insecure: this.settings.insecure,
      token: this.settings.token || undefined,
    };
  }

  private async fetchDocument() {
    const modal = new UrlInputModal(this.app, this.settings.serverUrl, async (url) => {
      try {
        const result = await cli.fetch(this.cliOpts(), url);
        if (result.status !== "ok") {
          new Notice(`Fetch failed: ${result.status}`);
          return;
        }

        const fileName = this.urlToFileName(url);
        const content =
          buildFrontmatter(url, result.version, result.etag) + result.body;

        const existing = this.app.vault.getAbstractFileByPath(fileName);
        if (existing instanceof TFile) {
          await this.app.vault.modify(existing, content);
          new Notice(`Updated: ${fileName}`);
        } else {
          const dir = fileName.substring(0, fileName.lastIndexOf("/"));
          if (dir) {
            await this.ensureDir(dir);
          }
          await this.app.vault.create(fileName, content);
          new Notice(`Created: ${fileName}`);
        }

        const file = this.app.vault.getAbstractFileByPath(fileName);
        if (file instanceof TFile) {
          await this.app.workspace.getLeaf().openFile(file);
        }
      } catch (e) {
        new Notice(`Error: ${e instanceof Error ? e.message : String(e)}`);
      }
    });
    modal.open();
  }

  private async publishNote() {
    const file = this.app.workspace.getActiveFile();
    if (!file) {
      new Notice("No active file");
      return;
    }

    const mark = getMarkFrontmatter(this.app, file);
    if (!mark) {
      const modal = new UrlInputModal(
        this.app,
        this.settings.serverUrl,
        async (url) => {
          await this.doPublish(file, url, -1);
        }
      );
      modal.open();
      return;
    }

    await this.doPublish(file, mark.url, mark.version);
  }

  private async doPublish(file: TFile, url: string, expectedVersion: number) {
    if (!this.settings.token) {
      new Notice("No token configured. Set one in Demarkus settings.");
      return;
    }

    try {
      const raw = await this.app.vault.read(file);
      const body = stripExistingFrontmatter(raw);

      const result = await cli.publish(
        this.cliOpts(),
        url,
        body,
        expectedVersion
      );

      if (result.status === "created" || result.status === "ok") {
        const newContent =
          buildFrontmatter(url, result.version, result.etag) + body;
        await this.app.vault.modify(file, newContent);
        new Notice(`Published v${result.version}: ${url}`);
      } else if (result.status === "conflict") {
        new Notice(
          `Conflict: server has a newer version. Fetch first to update.`
        );
      } else {
        new Notice(`Publish failed: ${result.status}`);
      }
    } catch (e) {
      new Notice(`Error: ${e instanceof Error ? e.message : String(e)}`);
    }
  }

  private async listDocuments() {
    const serverUrl = this.settings.serverUrl;
    if (!serverUrl) {
      new Notice("No server URL configured. Set one in Demarkus settings.");
      return;
    }

    try {
      const url = serverUrl.endsWith("/") ? serverUrl : serverUrl + "/";
      const entries = await cli.list(this.cliOpts(), url);

      const modal = new ListModal(this.app, entries, async (entry) => {
        if (entry.isDir) {
          new Notice("Directory browsing not yet supported");
          return;
        }
        const docUrl = url + entry.name;
        try {
          const result = await cli.fetch(this.cliOpts(), docUrl);
          if (result.status !== "ok") {
            new Notice(`Fetch failed: ${result.status}`);
            return;
          }

          const fileName = this.urlToFileName(docUrl);
          const content =
            buildFrontmatter(docUrl, result.version, result.etag) + result.body;

          const existing = this.app.vault.getAbstractFileByPath(fileName);
          if (existing instanceof TFile) {
            await this.app.vault.modify(existing, content);
          } else {
            const dir = fileName.substring(0, fileName.lastIndexOf("/"));
            if (dir) {
              await this.ensureDir(dir);
            }
            await this.app.vault.create(fileName, content);
          }

          const file = this.app.vault.getAbstractFileByPath(fileName);
          if (file instanceof TFile) {
            await this.app.workspace.getLeaf().openFile(file);
          }
          new Notice(`Fetched: ${fileName}`);
        } catch (e) {
          new Notice(`Error: ${e instanceof Error ? e.message : String(e)}`);
        }
      });
      modal.open();
    } catch (e) {
      new Notice(`Error: ${e instanceof Error ? e.message : String(e)}`);
    }
  }

  private urlToFileName(url: string): string {
    const match = url.match(/^mark:\/\/([^/]+)(\/.*)?$/);
    if (!match) return "demarkus-doc.md";
    const host = match[1];
    const path = match[2] || "/index.md";
    return `demarkus/${host}${path}`;
  }

  private async ensureDir(dir: string) {
    const parts = dir.split("/");
    let current = "";
    for (const part of parts) {
      current = current ? `${current}/${part}` : part;
      if (!this.app.vault.getAbstractFileByPath(current)) {
        await this.app.vault.createFolder(current);
      }
    }
  }

  async loadSettings() {
    this.settings = Object.assign({}, DEFAULT_SETTINGS, await this.loadData());
  }

  async saveSettings() {
    await this.saveData(this.settings);
  }
}

class UrlInputModal extends Modal {
  private url: string;
  private onSubmit: (url: string) => void;

  constructor(app: App, defaultUrl: string, onSubmit: (url: string) => void) {
    super(app);
    this.url = defaultUrl;
    this.onSubmit = onSubmit;
  }

  onOpen() {
    const { contentEl } = this;
    contentEl.createEl("h3", { text: "Demarkus URL" });

    new Setting(contentEl).setName("URL").addText((text) => {
      text.setPlaceholder("mark://host:port/path.md").setValue(this.url);
      text.onChange((value) => (this.url = value));
      text.inputEl.addEventListener("keydown", (e: KeyboardEvent) => {
        if (e.key === "Enter") {
          e.preventDefault();
          this.close();
          this.onSubmit(this.url);
        }
      });
      setTimeout(() => text.inputEl.focus(), 10);
    });

    new Setting(contentEl).addButton((btn) =>
      btn
        .setButtonText("Fetch")
        .setCta()
        .onClick(() => {
          this.close();
          this.onSubmit(this.url);
        })
    );
  }

  onClose() {
    this.contentEl.empty();
  }
}

class ListModal extends Modal {
  private entries: cli.ListEntry[];
  private onSelect: (entry: cli.ListEntry) => void;

  constructor(
    app: App,
    entries: cli.ListEntry[],
    onSelect: (entry: cli.ListEntry) => void
  ) {
    super(app);
    this.entries = entries;
    this.onSelect = onSelect;
  }

  onOpen() {
    const { contentEl } = this;
    contentEl.createEl("h3", { text: "Documents" });

    const list = contentEl.createEl("div", { cls: "demarkus-list" });
    for (const entry of this.entries) {
      const item = list.createEl("div", { cls: "demarkus-list-item" });
      const link = item.createEl("a", {
        text: entry.name,
        href: "#",
      });
      link.addEventListener("click", (e) => {
        e.preventDefault();
        this.close();
        this.onSelect(entry);
      });
    }
  }

  onClose() {
    this.contentEl.empty();
  }
}
