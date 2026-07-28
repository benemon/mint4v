{{- define "mint4v.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "mint4v.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "mint4v.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "mint4v.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Each of the three mounted Secrets can come from an existing Secret (the
*Secret value) or from an inline literal (the chart creates the Secret).
The two forms are mutually exclusive per item.
*/}}
{{- define "mint4v.vaultCASecretName" -}}
{{- if and .Values.vaultCASecret .Values.vaultCA -}}
{{- fail "set only one of vaultCASecret (existing Secret) and vaultCA (inline PEM)" -}}
{{- end -}}
{{- if .Values.vaultCASecret -}}{{ .Values.vaultCASecret }}{{- else -}}{{ include "mint4v.fullname" . }}-vault-ca{{- end -}}
{{- end -}}

{{- define "mint4v.targetCASecretName" -}}
{{- if and .Values.targetCASecret .Values.targetCA -}}
{{- fail "set only one of targetCASecret (existing Secret) and targetCA (inline PEM)" -}}
{{- end -}}
{{- if .Values.targetCASecret -}}{{ .Values.targetCASecret }}{{- else -}}{{ include "mint4v.fullname" . }}-target-ca{{- end -}}
{{- end -}}

{{- define "mint4v.credentialsSecretName" -}}
{{- if and .Values.credentialsSecret .Values.credentials -}}
{{- fail "set only one of credentialsSecret (existing Secret) and credentials (inline JSON)" -}}
{{- end -}}
{{- if .Values.credentialsSecret -}}{{ .Values.credentialsSecret }}{{- else -}}{{ include "mint4v.fullname" . }}-credentials{{- end -}}
{{- end -}}
