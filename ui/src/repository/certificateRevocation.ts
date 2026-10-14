import { type CertificateRevocationModel } from "@/domain/certificate";

import { COLLECTION_NAME_CERTIFICATE_REVOCATION, getPocketBase } from "./_pocketbase";

const pb = getPocketBase();
const pbco = pb.collection(COLLECTION_NAME_CERTIFICATE_REVOCATION);

const _commonFields = [
  "id",
  "certificateRef",
  "status",
  "stage",
  "errorType",
  "errorStatusCode",
  "errorMessage",
  "converged",
  "startedAt",
  "endedAt",
  "created",
  "updated",
];

const toLatestMap = (list: CertificateRevocationModel[]) => {
  // the query is sorted by -created, so the first item per certificate is the
  // latest attempt
  const map = new Map<string, CertificateRevocationModel>();
  for (const item of list) {
    if (!map.has(item.certificateRef)) {
      map.set(item.certificateRef, item);
    }
  }
  return map;
};

/// Fetches the latest revocation attempt for each of the given certificates.
export const listLatestByCertificateIds = async (certificateIds: string[]) => {
  if (certificateIds.length === 0) {
    return new Map<string, CertificateRevocationModel>();
  }

  const filters = certificateIds.map((id) => pb.filter("certificateRef={:id}", { id })).join(" || ");
  const list = await pbco.getFullList<CertificateRevocationModel>({
    batch: 65535,
    fields: _commonFields.join(","),
    filter: filters,
    sort: "-created",
    requestKey: null,
  });

  return toLatestMap(list);
};

/// Fetches the latest revocation attempt for a single certificate.
export const getLatestByCertificateId = async (certificateId: string) => {
  const list = await pbco.getFullList<CertificateRevocationModel>({
    batch: 100,
    fields: _commonFields.join(","),
    filter: pb.filter("certificateRef={:certificateId}", { certificateId }),
    sort: "-created",
    requestKey: null,
  });
  return list[0] ?? null;
};
