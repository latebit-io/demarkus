import { App, PluginSettingTab, Setting } from "obsidian";
import type DemarkusPlugin from "./main";

export interface DemarkusSettings {
  serverUrl: string;
  token: string;
  cliPath: string;
  insecure: boolean;
}

export const DEFAULT_SETTINGS: DemarkusSettings = {
  serverUrl: "",
  token: "",
  cliPath: "demarkus",
  insecure: false,
};

export class DemarkusSettingTab extends PluginSettingTab {
  plugin: DemarkusPlugin;

  constructor(app: App, plugin: DemarkusPlugin) {
    super(app, plugin);
    this.plugin = plugin;
  }

  display(): void {
    const { containerEl } = this;
    containerEl.empty();

    new Setting(containerEl)
      .setName("Server URL")
      .setDesc("Default demarkus server (e.g. mark://soul.demarkus.io)")
      .addText((text) =>
        text
          .setPlaceholder("mark://localhost:6309")
          .setValue(this.plugin.settings.serverUrl)
          .onChange(async (value) => {
            this.plugin.settings.serverUrl = value;
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName("Token")
      .setDesc("Capability token for publish/append operations")
      .addText((text) => {
        text
          .setPlaceholder("your-token-here")
          .setValue(this.plugin.settings.token)
          .onChange(async (value) => {
            this.plugin.settings.token = value;
            await this.plugin.saveSettings();
          });
        text.inputEl.type = "password";
      });

    new Setting(containerEl)
      .setName("CLI path")
      .setDesc("Path to demarkus binary")
      .addText((text) =>
        text
          .setPlaceholder("demarkus")
          .setValue(this.plugin.settings.cliPath)
          .onChange(async (value) => {
            this.plugin.settings.cliPath = value;
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName("Insecure")
      .setDesc("Skip TLS certificate verification (for local dev servers)")
      .addToggle((toggle) =>
        toggle
          .setValue(this.plugin.settings.insecure)
          .onChange(async (value) => {
            this.plugin.settings.insecure = value;
            await this.plugin.saveSettings();
          })
      );
  }
}
