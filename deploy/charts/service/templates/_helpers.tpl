{{/* The release name is the service name: Deployment, Service and ServiceAccount are all called this. */}}
{{- define "service.name" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Every pod of the release (server, PreSync Job, CronJobs): the NetworkPolicy selects these. */}}
{{- define "service.selectorLabels" -}}
app.kubernetes.io/name: {{ include "service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Only the long-running pods: Deployment, Service, PDB and spread constraints select these, never Job pods. */}}
{{- define "service.serverSelectorLabels" -}}
{{ include "service.selectorLabels" . }}
app.kubernetes.io/component: server
{{- end -}}

{{- define "service.labels" -}}
{{ include "service.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | quote }}
app.kubernetes.io/part-of: tasuki
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* Fail early, at render time, instead of shipping something the cluster will reject or kill. */}}
{{- define "service.validate" -}}
{{- if not .Values.image.repository -}}
{{- fail "image.repository is required" -}}
{{- end -}}
{{- if or (not .Values.image.tag) (eq (toString .Values.image.tag) "latest") -}}
{{- fail "image.tag is required and must not be \"latest\"" -}}
{{- end -}}
{{- $budget := add .Values.preStopSleepSeconds .Values.shutdownDrainSeconds -}}
{{- if gt (int $budget) (int .Values.terminationGracePeriodSeconds) -}}
{{- fail (printf "preStopSleepSeconds + shutdownDrainSeconds (%d s) exceeds terminationGracePeriodSeconds (%d s): the kubelet would SIGKILL mid-drain" (int $budget) (int .Values.terminationGracePeriodSeconds)) -}}
{{- end -}}
{{- end -}}

{{- define "service.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end -}}

{{/*
Environment variables: secrets first, then plain values, each sorted so renders are stable.
The OpenTelemetry defaults are merged under .Values.env, so a service can override any of them
(and no name appears twice, which server-side apply would reject).
*/}}
{{- define "service.env" -}}
{{- $env := deepCopy .Values.env -}}
{{- with .Values.otlp.endpoint -}}
{{- $env = merge $env (dict
      "OTEL_EXPORTER_OTLP_ENDPOINT" .
      "OTEL_SERVICE_NAME" (include "service.name" $)
      "OTEL_RESOURCE_ATTRIBUTES" (printf "deployment.environment.name=%s,service.version=%s" $.Release.Namespace (toString $.Values.image.tag))) -}}
{{- end -}}
{{- range $name := keys .Values.secretEnv | sortAlpha }}
{{- $ref := index $.Values.secretEnv $name }}
- name: {{ $name }}
  valueFrom:
    secretKeyRef:
      name: {{ required (printf "secretEnv.%s.secret is required" $name) $ref.secret }}
      key: {{ required (printf "secretEnv.%s.key is required" $name) $ref.key }}
{{- end }}
{{- range $name := keys $env | sortAlpha }}
- name: {{ $name }}
  value: {{ index $env $name | toString | quote }}
{{- end }}
{{- end -}}

{{/*
Pod spec shared by the Deployment, the PreSync Job and CronJobs.
Call with a dict: root (the chart context), args (overrides .Values.args when set),
long (true for the long-running Deployment: probes, preStop and the drain budget),
serviceAccount (overrides the release's own ServiceAccount).
*/}}
{{- define "service.podSpec" -}}
{{- $root := .root -}}
{{- $v := $root.Values -}}
serviceAccountName: {{ .serviceAccount | default (include "service.name" $root) }}
automountServiceAccountToken: {{ $v.serviceAccount.automountToken }}
enableServiceLinks: false
{{- with $v.priorityClassName }}
priorityClassName: {{ . }}
{{- end }}
securityContext:
  {{- toYaml $v.podSecurityContext | nindent 2 }}
{{- if .long }}
terminationGracePeriodSeconds: {{ $v.terminationGracePeriodSeconds }}
{{- else }}
restartPolicy: Never
{{- end }}
containers:
  - name: app
    image: {{ include "service.image" $root }}
    imagePullPolicy: {{ $v.image.pullPolicy }}
    {{- with $v.command }}
    command:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    {{- with (.args | default $v.args) }}
    args:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    {{- if .long }}
    ports:
      - name: http
        containerPort: {{ $v.port }}
        protocol: TCP
      {{- if $v.metrics.port }}
      - name: metrics
        containerPort: {{ $v.metrics.port }}
        protocol: TCP
      {{- end }}
    {{- end }}
    {{- with (include "service.env" $root | trim) }}
    env:
      {{- . | nindent 6 }}
    {{- end }}
    {{- if .long }}
    {{- with $v.probes.startup }}
    startupProbe:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    livenessProbe:
      {{- toYaml (required "probes.liveness is required" $v.probes.liveness) | nindent 6 }}
    readinessProbe:
      {{- toYaml (required "probes.readiness is required" $v.probes.readiness) | nindent 6 }}
    lifecycle:
      preStop:
        # Built-in sleep action (no shell needed in distroless images): keep serving while the endpoint
        # is removed from Services and the ALB, then SIGTERM starts the app's own drain.
        sleep:
          seconds: {{ $v.preStopSleepSeconds }}
    {{- end }}
    resources:
      {{- toYaml $v.resources | nindent 6 }}
    securityContext:
      {{- toYaml $v.securityContext | nindent 6 }}
    volumeMounts:
      - name: tmp
        mountPath: /tmp
volumes:
  - name: tmp
    emptyDir:
      sizeLimit: {{ $v.tmpVolume.sizeLimit }}
{{- with $v.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $v.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $v.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}
