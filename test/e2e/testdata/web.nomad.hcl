job "e2e-web" {
  group "web" {
    network {
      mode = "bridge"
      port "http" {
        to = 8080
      }
    }
    service {
      name     = "e2e-web"
      provider = "nomad"
      port     = "http"
      check {
        type     = "http"
        path     = "/"
        interval = "5s"
        timeout  = "2s"
      }
    }
    task "web" {
      driver = "docker"
      config {
        image   = "busybox:1.38"
        command = "sh"
        args    = ["-c", "mkdir -p /www && echo hello >/www/index.html && exec httpd -f -p 8080 -h /www"]
        ports   = ["http"]
      }
      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
