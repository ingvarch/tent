job "e2e-metadata" {
  type = "batch"

  group "nomad-bridge" {
    network {
      mode = "bridge"
    }
    restart {
      attempts = 0
      mode     = "fail"
    }
    reschedule {
      attempts  = 0
      unlimited = false
    }
    task "probe" {
      driver = "docker"
      config {
        image   = "busybox:1.38"
        command = "sh"
        args    = ["/local/probe.sh"]
      }
      template {
        destination = "local/probe.sh"
        data        = <<-EOT
          control=""
          for url in http://deb.debian.org/debian/ http://detectportal.firefox.com/success.txt \
            http://captive.apple.com/hotspot-detect.html; do
            if wget -q -T 10 -O /dev/null "$url"; then
              control="$url"
              break
            fi
          done
          [ -n "$control" ] || { echo control-failed >&2; exit 12; }
          if o=$(wget -q -T 5 -O /dev/null http://169.254.169.254/v1.json 2>&1); then
            echo reached >&2
            exit 10
          fi
          echo "$o" >&2
          case "$o" in
            *"timed out"*) exit 0 ;;
          esac
          exit 11
        EOT
      }
      resources {
        cpu    = 50
        memory = 32
      }
    }
  }

  group "docker-bridge" {
    restart {
      attempts = 0
      mode     = "fail"
    }
    reschedule {
      attempts  = 0
      unlimited = false
    }
    task "probe" {
      driver = "docker"
      config {
        image        = "busybox:1.38"
        network_mode = "bridge"
        command      = "sh"
        args         = ["/local/probe.sh"]
      }
      template {
        destination = "local/probe.sh"
        data        = <<-EOT
          control=""
          for url in http://deb.debian.org/debian/ http://detectportal.firefox.com/success.txt \
            http://captive.apple.com/hotspot-detect.html; do
            if wget -q -T 10 -O /dev/null "$url"; then
              control="$url"
              break
            fi
          done
          [ -n "$control" ] || { echo control-failed >&2; exit 12; }
          if o=$(wget -q -T 5 -O /dev/null http://169.254.169.254/v1.json 2>&1); then
            echo reached >&2
            exit 10
          fi
          echo "$o" >&2
          case "$o" in
            *"timed out"*) exit 0 ;;
          esac
          exit 11
        EOT
      }
      resources {
        cpu    = 50
        memory = 32
      }
    }
  }

  group "host" {
    restart {
      attempts = 0
      mode     = "fail"
    }
    reschedule {
      attempts  = 0
      unlimited = false
    }
    task "probe" {
      driver = "docker"
      config {
        image        = "busybox:1.38"
        network_mode = "host"
        command      = "sh"
        args         = ["/local/probe.sh"]
      }
      template {
        destination = "local/probe.sh"
        data        = <<-EOT
          control=""
          for url in http://deb.debian.org/debian/ http://detectportal.firefox.com/success.txt \
            http://captive.apple.com/hotspot-detect.html; do
            if wget -q -T 10 -O /dev/null "$url"; then
              control="$url"
              break
            fi
          done
          [ -n "$control" ] || { echo control-failed >&2; exit 12; }
          if o=$(wget -q -T 5 -O /dev/null http://169.254.169.254/v1.json 2>&1); then
            echo reached >&2
            exit 10
          fi
          echo "$o" >&2
          case "$o" in
            *"timed out"*) exit 0 ;;
          esac
          exit 11
        EOT
      }
      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
