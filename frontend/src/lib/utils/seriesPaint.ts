import type { SeriesStyle } from "@kenn-io/kit-ui/utils/palette";

type SeriesFillStyle = Pick<SeriesStyle, "color" | "fill">;

/** The id of the SVG pattern SeriesPatternDefs draws for a patterned
 * style. `scope` keeps ids unique when several charts share a page. */
export function seriesPatternId(scope: string, style: SeriesFillStyle): string {
  return `${scope}-${style.fill}-${style.color.replace(/[^a-z0-9]/gi, "")}`;
}

/** An SVG fill for a series: its color, or a reference to its pattern. */
export function seriesPaint(scope: string, style: SeriesFillStyle): string {
  return style.fill === "solid" ? style.color : `url(#${seriesPatternId(scope, style)})`;
}
