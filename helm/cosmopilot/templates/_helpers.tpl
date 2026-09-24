{{- define "ports.service" }}
{{- range $key, $value := .Values.ports }}
- port: {{ $v := $value | toString | splitList ":" }}{{$v | first}}
  name: {{ $key }}
  targetPort: {{ $v := $value | toString | splitList ":" }}{{$v | last}}
{{- end }}
{{- end }}

{{- define "ports.pod" }}
{{- range $key, $value := .Values.ports }}
  - containerPort: {{ $v := $value | toString | splitList ":" }}{{$v | last}}
    name: {{ $key }}
{{- end }}
{{- end }}

{{- define "cosmopilot.selectorLabels" }}
app.kubernetes.io/name: {{ .Chart.Name }}
helm.sh/chart: {{ .Chart.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "cosmopilot.labels" }}
{{- include "cosmopilot.selectorLabels" . }}
{{- with omit (.Values.labels | default dict) "app.kubernetes.io/name" "helm.sh/chart" "app.kubernetes.io/managed-by" "app.kubernetes.io/instance" }}
{{ toYaml . | trimSuffix "\n" }}
{{- end }}
{{- end }}