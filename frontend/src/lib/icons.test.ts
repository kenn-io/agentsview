import { cleanup, render } from "@testing-library/svelte";
import { afterEach, describe, expect, it } from "vitest";

import * as icons from "./icons.ts";

const approvedIconNames = [
  "ActivityIcon",
  "AlignJustifyIcon",
  "ArrowDownIcon",
  "ArrowDownWideNarrowIcon",
  "ArrowUpIcon",
  "ArrowUpNarrowWideIcon",
  "CalendarIcon",
  "CheckIcon",
  "ChartColumnIcon",
  "ChevronDownIcon",
  "ChevronLeftIcon",
  "ChevronRightIcon",
  "ChevronUpIcon",
  "CirclePlayIcon",
  "CircleQuestionMarkIcon",
  "ClockIcon",
  "CodeIcon",
  "CloudUploadIcon",
  "CopyIcon",
  "DatabaseBackupIcon",
  "DownloadIcon",
  "EllipsisIcon",
  "EllipsisVerticalIcon",
  "ExternalLinkIcon",
  "FileCheckIcon",
  "FileIcon",
  "FileTextIcon",
  "FileXIcon",
  "FolderIcon",
  "FunnelIcon",
  "GlobeIcon",
  "Grid2x2Icon",
  "LayoutGridIcon",
  "LayoutListIcon",
  "LightbulbIcon",
  "LinkIcon",
  "ListCollapseIcon",
  "LockIcon",
  "LogsIcon",
  "MenuIcon",
  "MessageSquareIcon",
  "MessageSquareTextIcon",
  "MonitorIcon",
  "MoonIcon",
  "MoreHorizontalIcon",
  "MousePointer2Icon",
  "PencilIcon",
  "PinIcon",
  "PlusIcon",
  "RefreshCwIcon",
  "SearchIcon",
  "SettingsIcon",
  "SquareTerminalIcon",
  "StarIcon",
  "SunIcon",
  "TrashIcon",
  "TriangleAlertIcon",
  "UploadIcon",
  "UserRoundIcon",
  "UsersRoundIcon",
  "XIcon",
] as const;

describe("icons barrel", () => {
  afterEach(() => {
    cleanup();
  });

  it("exports the approved app icon set", () => {
    expect(Object.keys(icons).sort()).toEqual([...approvedIconNames].sort());
  });

  it("renders each approved icon as an svg", () => {
    for (const name of approvedIconNames) {
      const IconComponent = icons[name];
      const { container, unmount } = render(IconComponent, {
        props: {
          size: "16",
          "aria-hidden": "true",
        },
      });

      expect(container.querySelector("svg"), `${name} should render an svg`).toBeTruthy();
      unmount();
    }
  });
});
