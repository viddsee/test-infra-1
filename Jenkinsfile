pipeline {
    agent {
        label "jenkins-go"
    }
    stages {
        stage('CI Build and Test') {
            when {
                branch 'PR-*'
            }
            steps {
                dir ('/home/jenkins/go/src/k8s.io/test-infra') {
                    checkout scm
                    container('go') {
                        sh "./hack/build-then-unit.sh"
                    }
                }
            }
        }
         stage('Build and Release') {
            when {
                branch 'master'
            }
            steps {
                dir ('/home/jenkins/go/src/k8s.io/test-infra') {
                    checkout scm
                    container('go') {
                        sh "git config --global credential.helper store"
                        sh "jx step git credentials"
                        sh "jx step next-version --use-git-tag-only --tag"
                        sh "./hack/build-then-unit.sh"
                        sh "./hack/jx-docker-build.sh \$(cat VERSION)"
                    }
                }
            }
        }
    }
}