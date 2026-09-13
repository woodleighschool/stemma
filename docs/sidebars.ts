import type { SidebarsConfig } from "@docusaurus/plugin-content-docs";

const sidebars: SidebarsConfig = {
  docsSidebar: [
    "index",
    "getting-started",
    {
      type: "category",
      label: "Using Stemma",
      items: [
        "catalogs",
        "sources",
        "mac-software",
        "building-packages",
        "windows-software",
        "publishing",
        "plugins",
      ],
    },
    { type: "category", label: "Reference", items: ["commands", "limitations"] },
    "writing-plugins",
    "development",
  ],
};

export default sidebars;
