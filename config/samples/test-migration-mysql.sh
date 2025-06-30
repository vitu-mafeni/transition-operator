#!/bin/bash

# ----------- USER CONFIGURATION ------------
DEPLOYMENT_NAME="wordpress-mysql"
NAMESPACE="wordpress"
APP_LABEL="wordpress"
MYSQL_LABEL_TIER="mysql"
MYSQL_PORT=3306
CLUSTER_A_IP=""
CLUSTER_B_IP=""
MYSQL_NODEPORT=""

KUBECONFIG_A="./edge.kubeconfig"
KUBECONFIG_B="./core.kubeconfig"
TIMEOUT=1
OVERALL_TIMEOUT=600

# ----------- LOG SETUP ------------
LOG_DIR="./migration_logs"
mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/mysql_migration_log_$(date +%Y%m%d_%H%M%S).log"

log() {
  echo "[$(date +'%F %T')] $1" | tee -a "$LOG_FILE"
}

log_event_gap() {
  local label="$1"
  local t1="$2"
  local t2="$3"
  if [ "$t1" -ne 0 ] && [ "$t2" -ne 0 ]; then
    echo "$((t2 - t1))s" | awk -v msg="$label" '{ printf("%-40s: %s\n", msg, $0) }' | tee -a "$LOG_FILE"
  fi
}

log_pod_status() {
  local cluster=$1
  local kubeconfig=$2
  local phase=$3
  log "[$cluster] --- POD STATUS SNAPSHOT during $phase ---"
  kubectl --kubeconfig="$kubeconfig" get pods -n "$NAMESPACE" -l app="$APP_LABEL${MYSQL_LABEL_TIER:+,tier=$MYSQL_LABEL_TIER}" -o wide | tee -a "$LOG_FILE"
}

# ----------- MAIN LOOP (Continuous Monitoring) ------------
while true; do
  declare -A events
  for cluster in A B; do
    events["deploy_$cluster"]=0
    events["running_$cluster"]=0
    events["ready_$cluster"]=0
    events["service_$cluster"]=0
  done

  START_TIME=$(date +%s)
  log "Starting MySQL migration monitoring..."
  log "Tracking deployment: $DEPLOYMENT_NAME in namespace: $NAMESPACE"

  while true; do
    NOW=$(date +%s)
    if (( NOW - START_TIME > OVERALL_TIMEOUT )); then
      log "ERROR: Monitoring timed out after $OVERALL_TIMEOUT seconds."
      break
    fi

    # ---- Cluster A ----
    if [ "${events[deploy_A]}" -eq 0 ]; then
      if kubectl --kubeconfig="$KUBECONFIG_A" get deployment "$DEPLOYMENT_NAME" -n "$NAMESPACE" &>/dev/null; then
        events[deploy_A]=$NOW
        log "[A] Deployment detected at $(date -d @$NOW)"
      fi
    fi

    if [ "${events[running_A]}" -eq 0 ]; then
      PHASE=$(kubectl --kubeconfig="$KUBECONFIG_A" get pods -n "$NAMESPACE" -l app="$APP_LABEL${MYSQL_LABEL_TIER:+,tier=$MYSQL_LABEL_TIER}" -o jsonpath='{.items[*].status.phase}')
      if echo "$PHASE" | grep -qw "Running"; then
        events[running_A]=$NOW
        log "[A] Pod is Running at $(date -d @$NOW)"
      fi
    fi

    if [ "${events[ready_A]}" -eq 0 ]; then
      READY=$(kubectl --kubeconfig="$KUBECONFIG_A" get pods -n "$NAMESPACE" -l app="$APP_LABEL${MYSQL_LABEL_TIER:+,tier=$MYSQL_LABEL_TIER}" -o jsonpath='{.items[*].status.conditions[?(@.type=="Ready")].status}')
      if echo "$READY" | grep -qw "True"; then
        events[ready_A]=$NOW
        log "[A] Pod is Ready at $(date -d @$NOW)"
      fi
    fi

    # ---- Cluster B ----
    if [ "${events[deploy_B]}" -eq 0 ]; then
      if kubectl --kubeconfig="$KUBECONFIG_B" get deployment "$DEPLOYMENT_NAME" -n "$NAMESPACE" &>/dev/null; then
        events[deploy_B]=$NOW
        log "[B] Deployment detected at $(date -d @$NOW)"
      fi
    fi

    if [ "${events[running_B]}" -eq 0 ]; then
      PHASE=$(kubectl --kubeconfig="$KUBECONFIG_B" get pods -n "$NAMESPACE" -l app="$APP_LABEL${MYSQL_LABEL_TIER:+,tier=$MYSQL_LABEL_TIER}" -o jsonpath='{.items[*].status.phase}')
      if echo "$PHASE" | grep -qw "Running"; then
        events[running_B]=$NOW
        log "[B] Pod is Running at $(date -d @$NOW)"
      fi
    fi

    if [ "${events[ready_B]}" -eq 0 ]; then
      READY=$(kubectl --kubeconfig="$KUBECONFIG_B" get pods -n "$NAMESPACE" -l app="$APP_LABEL${MYSQL_LABEL_TIER:+,tier=$MYSQL_LABEL_TIER}" -o jsonpath='{.items[*].status.conditions[?(@.type=="Ready")].status}')
      if echo "$READY" | grep -qw "True"; then
        events[ready_B]=$NOW
        log "[B] Pod is Ready at $(date -d @$NOW)"
      fi
    fi

    # ---- TCP Check (Optional) ----
    if [ -n "$CLUSTER_B_IP" ] && [ -n "$MYSQL_NODEPORT" ] && [ "${events[service_B]}" -eq 0 ]; then
      timeout $TIMEOUT bash -c "cat < /dev/null > /dev/tcp/$CLUSTER_B_IP/$MYSQL_NODEPORT" 2>/dev/null
      if [ $? -eq 0 ]; then
        events[service_B]=$NOW
        log "[B] MySQL TCP port $MYSQL_NODEPORT is OPEN at $(date -d @$NOW)"
        break
      fi
    elif [ "${events[ready_B]}" -ne 0 ]; then
      break
    fi

    sleep 1
  done

  # ----------- SUMMARY REPORT ------------
  log "--------- MYSQL MIGRATION SUMMARY ---------"
  for cluster in A B; do
    for phase in deploy running ready service; do
      TS=${events["${phase}_$cluster"]}
      if [ "$TS" -ne 0 ]; then
        log "[$cluster] ${phase^}: $(date -d @$TS)"
      fi
    done
  done

  log "--------- TIME GAPS BETWEEN EVENTS ---------"
  log_event_gap "[A] Deploy -> Running" ${events[deploy_A]} ${events[running_A]}
  log_event_gap "[A] Running -> Ready" ${events[running_A]} ${events[ready_A]}
  log_event_gap "[A] Ready -> B Deploy" ${events[ready_A]} ${events[deploy_B]}
  log_event_gap "[B] Deploy -> Running" ${events[deploy_B]} ${events[running_B]}
  log_event_gap "[B] Running -> Ready" ${events[running_B]} ${events[ready_B]}
  log_event_gap "[B] Ready -> Service" ${events[ready_B]} ${events[service_B]}

  if [ "${events[ready_A]}" -ne 0 ] && [ "${events[ready_B]}" -ne 0 ]; then
    downtime=$((events[ready_B] - events[ready_A]))
    log "[Total] Estimated backend readiness downtime: $downtime seconds"
  fi

  log "MySQL migration monitoring cycle completed."
  log "Waiting 1 second before restarting monitoring..."
  log "----------------------------------------"
  echo ""; echo ""
  sleep 1

done
