import type * as Preset from "@docusaurus/preset-classic";
import type { Config } from "@docusaurus/types";

import type { PrismTheme } from "prism-react-renderer";

const lightCodeTheme: PrismTheme = {
  plain: {
    color: "oklch(0.25 0.006 80)",
    backgroundColor: "oklch(0.925 0.009 82)",
  },
  styles: [
    { types: ["comment", "prolog", "doctype", "cdata"], style: { color: "#6c756e" } },
    { types: ["punctuation"], style: { color: "#59635c" } },
    {
      types: ["property", "tag", "boolean", "number", "constant", "symbol"],
      style: { color: "#23694d" },
    },
    { types: ["selector", "attr-name", "string", "char", "builtin"], style: { color: "#526b2d" } },
    { types: ["operator", "entity", "url"], style: { color: "#59635c" } },
    { types: ["atrule", "attr-value", "keyword"], style: { color: "#715b20" } },
    { types: ["function", "class-name"], style: { color: "#9a3b2f" } },
    { types: ["regex", "important", "variable"], style: { color: "#8a5a1c" } },
  ],
};

const darkCodeTheme: PrismTheme = {
  plain: {
    color: "oklch(0.9 0.006 82)",
    backgroundColor: "oklch(0.295 0.006 80)",
  },
  styles: [
    { types: ["comment", "prolog", "doctype", "cdata"], style: { color: "#8b938d" } },
    { types: ["punctuation"], style: { color: "#b7beb8" } },
    {
      types: ["property", "tag", "boolean", "number", "constant", "symbol"],
      style: { color: "#82caa2" },
    },
    { types: ["selector", "attr-name", "string", "char", "builtin"], style: { color: "#bfd483" } },
    { types: ["operator", "entity", "url"], style: { color: "#b7beb8" } },
    { types: ["atrule", "attr-value", "keyword"], style: { color: "#dfc66b" } },
    { types: ["function", "class-name"], style: { color: "#e99482" } },
    { types: ["regex", "important", "variable"], style: { color: "#e2af63" } },
  ],
};

const config: Config = {
  title: "Stemma",
  tagline: "Cross-platform software artifact pipeline.",
  future: { v4: true },
  url: process.env.DOCS_URL || "https://woodleighschool.github.io",
  baseUrl: process.env.DOCS_BASE_URL || "/stemma/",
  trailingSlash: false,
  organizationName: "woodleighschool",
  projectName: "stemma",
  onBrokenLinks: "throw",
  markdown: { hooks: { onBrokenMarkdownLinks: "throw" } },
  i18n: { defaultLocale: "en", locales: ["en"] },
  themes: [
    [
      "@easyops-cn/docusaurus-search-local",
      {
        hashed: true,
        indexBlog: false,
        docsRouteBasePath: "/",
        docsDir: "content",
        language: "en",
        searchBarShortcutHint: false,
      },
    ],
  ],
  plugins: [
    [
      "docusaurus-plugin-llms",
      {
        docsDir: [{ path: "content", routeBasePath: "/" }],
        generateLLMsTxt: true,
        generateLLMsFullTxt: true,
        includeBlog: false,
        excludeImports: true,
        removeDuplicateHeadings: true,
      },
    ],
  ],
  presets: [
    [
      "classic",
      {
        docs: {
          path: "content",
          sidebarPath: "./sidebars.ts",
          routeBasePath: "/",
          editUrl: "https://github.com/woodleighschool/stemma/tree/main/docs/",
        },
        blog: false,
      } satisfies Preset.Options,
    ],
  ],
  themeConfig: {
    colorMode: { defaultMode: "dark", respectPrefersColorScheme: true },
    navbar: {
      title: "Stemma",
      items: [
        { type: "docSidebar", sidebarId: "docsSidebar", position: "left", label: "Docs" },
        { href: "https://github.com/woodleighschool/stemma", position: "right", label: "GitHub" },
      ],
    },
    footer: {
      style: "dark",
      links: [
        {
          title: "Docs",
          items: [
            { label: "Getting started", to: "/getting-started" },
            { label: "Writing a catalog", to: "/catalogs" },
            { label: "Writing plugins", to: "/writing-plugins" },
          ],
        },
        {
          title: "Project",
          items: [
            { label: "Repository", href: "https://github.com/woodleighschool/stemma" },
            { label: "Catalog", href: "https://github.com/woodleighschool/stemma-catalog" },
          ],
        },
      ],
    },
    prism: {
      theme: lightCodeTheme,
      darkTheme: darkCodeTheme,
      additionalLanguages: ["bash", "go", "json", "powershell", "toml", "yaml"],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
