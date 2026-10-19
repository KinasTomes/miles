resource "aws_lb" "this" {
  count                      = var.enabled ? 1 : 0
  name                       = var.name
  internal                   = false
  load_balancer_type         = "application"
  security_groups            = [var.security_group_id]
  subnets                    = var.subnet_ids
  idle_timeout               = 120
  preserve_host_header       = true
  drop_invalid_header_fields = false # Preserve valid HTTP metadata header names (e.g. underscores) used by SigV4.
  lifecycle {
    precondition {
      condition     = var.certificate_arn != null && length(var.client_cidrs) > 0
      error_message = "ALB requires a real issued ACM certificate ARN and non-empty client CIDR allowlist."
    }
  }
}
resource "aws_lb_target_group" "s3" {
  count                = var.enabled ? 1 : 0
  name                 = "${var.name}-s3"
  port                 = 8080
  protocol             = "HTTP"
  target_type          = "instance"
  vpc_id               = var.vpc_id
  deregistration_delay = 1800
  health_check {
    path     = "/readyz"
    port     = "9100"
    protocol = "HTTP"
    matcher  = "200"
  }
}
resource "aws_lb_listener" "https" {
  count             = var.enabled ? 1 : 0
  load_balancer_arn = aws_lb.this[0].arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = var.certificate_arn
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.s3[0].arn
  }
}
output "dns_name" { value = try(aws_lb.this[0].dns_name, null) }
output "target_group_arn" { value = try(aws_lb_target_group.s3[0].arn, null) }
output "contract" {
  value = var.enabled ? {
    preserve_host_header = aws_lb.this[0].preserve_host_header
    idle_timeout         = aws_lb.this[0].idle_timeout
    listener_protocol    = aws_lb_listener.https[0].protocol
    listener_port        = aws_lb_listener.https[0].port
    target_port          = aws_lb_target_group.s3[0].port
    health_port          = aws_lb_target_group.s3[0].health_check[0].port
    health_path          = aws_lb_target_group.s3[0].health_check[0].path
    drain_seconds        = aws_lb_target_group.s3[0].deregistration_delay
  } : null
}
