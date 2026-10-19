provider "aws" {
  region              = var.region
  allowed_account_ids = [var.account_id]
  dynamic "assume_role" {
    for_each = var.deploy_role_arn == null ? [] : [var.deploy_role_arn]
    content { role_arn = assume_role.value }
  }
  default_tags { tags = { Project = "Miles", Environment = "dev", Owner = "Hung", ManagedBy = "Terraform" } }
}
