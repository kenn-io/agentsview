/* generated using openapi-typescript-codegen -- do not edit */
/* istanbul ignore file */
/* tslint:disable */
/* eslint-disable */
export type IssueReviewFindingStateInputBody = {
  /**
   * Finding last_seen snapshot
   */
  finding_last_seen: string;
  /**
   * Accepted finding state
   */
  review_state: IssueReviewFindingStateInputBody.review_state;
  /**
   * Suppress for 1, 7, or 30 days; omit for permanent suppression
   */
  suppression_days?: number;
};
export namespace IssueReviewFindingStateInputBody {
  /**
   * Accepted finding state
   */
  export enum review_state {
    ACKNOWLEDGED = 'acknowledged',
    SUPPRESSED = 'suppressed',
  }
}
