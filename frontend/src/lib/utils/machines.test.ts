import { describe, expect, it } from "vite-plus/test";
import { sessionMachineDisplay } from "./machines.js";

describe("sessionMachineDisplay", () => {
  it("keeps all contributing device labels and identifies duplicate labels", () => {
    expect(
      sessionMachineDisplay(
        {
          machine: "hosted",
          source_machines: ["raw-device:61", "raw-device:62", "raw-device:61"],
        },
        {
          "raw-device:61": "Workstation",
          "raw-device:62": "Workstation",
        },
      ),
    ).toEqual({
      ids: ["raw-device:61", "raw-device:62"],
      label: "Workstation, Workstation",
      title: "Workstation (raw-device:61), Workstation (raw-device:62)",
    });
  });

  it.each([undefined, []])("falls back to the legacy machine for %j sources", (source_machines) => {
    expect(
      sessionMachineDisplay(
        { machine: "installation-a", source_machines },
        {
          "installation-a": "Laptop",
        },
      ),
    ).toEqual({ ids: ["installation-a"], label: "Laptop", title: "Laptop (installation-a)" });
  });

  it("uses stable IDs when device labels are unavailable", () => {
    expect(
      sessionMachineDisplay({ machine: "hosted", source_machines: ["raw-device:61"] }, {}),
    ).toEqual({ ids: ["raw-device:61"], label: "raw-device:61", title: "raw-device:61" });
  });
});
