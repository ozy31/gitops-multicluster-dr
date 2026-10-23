{{/*
Fully qualified name. The ApplicationSet sets the Helm release name to the
app directory name (apps/<name>), so objects are named after the app.
*/}}
{{- define "service.fullname" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "service.selectorLabels" -}}
app.kubernetes.io/name: {{ include "service.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "service.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "service.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: gitops-multicluster-dr
dr.example.com/env: {{ .Values.site.env | quote }}
dr.example.com/role: {{ .Values.site.role | quote }}
dr.example.com/active: {{ include "service.isActive" . | quote }}
{{- end }}

{{- define "service.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "service.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
service.isActive renders "true" when this site should run full capacity:
- the app is not part of a DR pair (dev, stage), or
- dr.activeSite is "both" (a switch is in progress), or
- dr.activeSite equals this cluster's role.
*/}}
{{- define "service.isActive" -}}
{{- $active := .Values.dr.activeSite | toString -}}
{{- if not (has $active (list "primary" "dr" "both")) -}}
{{- fail (printf "dr.activeSite must be primary, dr or both; got %q" $active) -}}
{{- end -}}
{{- if not (has (.Values.site.role | toString) (list "primary" "dr")) -}}
{{- fail (printf "site.role must be primary or dr; got %q" .Values.site.role) -}}
{{- end -}}
{{- if or (not .Values.dr.managed) (eq $active "both") (eq $active .Values.site.role) -}}
true
{{- else -}}
false
{{- end -}}
{{- end }}

{{- define "service.autoscaled" -}}
{{- if or .Values.autoscaling.hpa.enabled .Values.autoscaling.keda.enabled -}}
true
{{- else -}}
false
{{- end -}}
{{- end }}
