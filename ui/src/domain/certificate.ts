import { type WorkflowModel } from "./workflow";

export interface CertificateModel extends BaseModel {
  source: string;
  certificate: string;
  privateKey: string;
  serialNumber: string;
  subjectName: string;
  subjectAltNames: string;
  issuerName: string;
  issuerOrg: string;
  keyAlgorithm: string;
  validationPolicy: string;
  validityNotBefore: ISO8601String;
  validityNotAfter: ISO8601String;
  isRenewed: boolean;
  isRevoked: boolean;
  workflowRef: string;
  /** UI-only: latest revocation attempt state joined from certificate_revocation. */
  revokeStatus?: CertificateRevocationStatusType;
  revokeErrorMessage?: string;
  revokeEndedAt?: ISO8601String;
  expand?: {
    workflowRef?: Pick<WorkflowModel, "id" | "name" | "description">;
  };
}

export interface CertificateRevocationModel extends BaseModel {
  certificateRef: string;
  status: CertificateRevocationStatusType;
  stage?: string;
  errorType?: string;
  errorStatusCode?: number;
  errorMessage?: string;
  converged: boolean;
  startedAt?: ISO8601String;
  endedAt?: ISO8601String;
}

export const CERTIFICATE_REVOCATION_STATUS = Object.freeze({
  PROCESSING: "processing",
  SUCCEEDED: "succeeded",
  FAILED: "failed",
  UNKNOWN: "unknown",
} as const);

export type CertificateRevocationStatusType = (typeof CERTIFICATE_REVOCATION_STATUS)[keyof typeof CERTIFICATE_REVOCATION_STATUS];

export const CERTIFICATE_SOURCES = Object.freeze({
  REQUEST: "request",
  UPLOAD: "upload",
} as const);

export type CertificateSourceType = (typeof CERTIFICATE_SOURCES)[keyof typeof CERTIFICATE_SOURCES];

export const CERTIFICATE_FORMATS = Object.freeze({
  PEM: "PEM",
  PFX: "PFX",
  JKS: "JKS",
} as const);

export type CertificateFormatType = (typeof CERTIFICATE_FORMATS)[keyof typeof CERTIFICATE_FORMATS];
