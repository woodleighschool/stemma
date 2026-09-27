import type { SidebarsConfig } from "@docusaurus/plugin-content-docs";

const sidebars: SidebarsConfig = {
  docsSidebar: [
    "index",
    "getting-started",
    {
      type: "category",
      label: "Catalogs",
      items: ["catalogs", "sources", "expressions"],
    },
    {
      type: "category",
      label: "Resource kinds",
      items: ["mac-software", "windows-software", "building-packages"],
    },
    {
      type: "category",
      label: "Publishing",
      items: ["publishing", "intune-windows", "reconcile"],
    },
    {
      type: "category",
      label: "Plugins",
      items: ["plugins", "writing-plugins"],
    },
    { type: "category", label: "Reference", items: ["commands", "limitations"] },
    "development",
  ],
};

export default sidebars;
