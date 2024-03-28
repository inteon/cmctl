/*
Copyright 2020 The cert-manager Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package secret

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/template"
	"time"

	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/cert-manager/cert-manager/pkg/util/pki"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	k8sclock "k8s.io/utils/clock"

	"github.com/cert-manager/cmctl/v2/pkg/build"
	"github.com/cert-manager/cmctl/v2/pkg/factory"
)

var clock k8sclock.Clock = k8sclock.RealClock{}

const validForTemplate = `Valid for:
	DNS Names: {{ .DNSNames }}
	URIs: {{ .URIs }}
	IP Addresses: {{ .IPAddresses }}
	Email Addresses: {{ .EmailAddresses }}
	Usages: {{ .KeyUsage }}`

const validityPeriodTemplate = `Validity period:
	Not Before: {{ .NotBefore }}
	Not After: {{ .NotAfter }}`

const issuedByTemplate = `Issued By:
	Common Name:	{{ .CommonName }}
	Organization:	{{ .Organization }}
	OrganizationalUnit:	{{ .OrganizationalUnit }}
	Country:	{{ .Country }}`

const issuedForTemplate = `Issued For:
	Common Name:	{{ .CommonName }}
	Organization:	{{ .Organization }}
	OrganizationalUnit:	{{ .OrganizationalUnit }}
	Country:	{{ .Country }}`

const certificateTemplate = `Certificate:
	Signing Algorithm:	{{ .SigningAlgorithm }}
	Public Key Algorithm: 	{{ .PublicKeyAlgorithm }}
	Serial Number:	{{ .SerialNumber }}
	Fingerprints: 	{{ .Fingerprints }}
	Is a CA certificate: {{ .IsCACertificate }}
	CRL:	{{ .CRL }}
	OCSP:	{{ .OCSP }}`

const debuggingTemplate = `Debugging:
	Trusted by this computer:	{{ .TrustedByThisComputer }}
	CRL Status:	{{ .CRLStatus }}
	OCSP Status:	{{ .OCSPStatus }}`

var (
	long = templates.LongDesc(i18n.T(`
Get details about a kubernetes.io/tls typed secret`))

	example = templates.Examples(i18n.T(build.WithTemplate(`
# Query information about a secret with name 'my-crt' in namespace 'my-namespace'
{{.BuildName}} inspect secret my-crt --namespace my-namespace
`)))
)

// Options is a struct to support status certificate command
type Options struct {
	genericclioptions.IOStreams
	*factory.Factory
}

// NewOptions returns initialized Options
func NewOptions(ioStreams genericclioptions.IOStreams) *Options {
	return &Options{
		IOStreams: ioStreams,
	}
}

// NewCmdInspectSecret returns a cobra command for status certificate
func NewCmdInspectSecret(setupCtx context.Context, ioStreams genericclioptions.IOStreams) *cobra.Command {
	o := NewOptions(ioStreams)

	cmd := &cobra.Command{
		Use:               "secret",
		Short:             "Get details about a kubernetes.io/tls typed secret",
		Long:              long,
		Example:           example,
		ValidArgsFunction: factory.ValidArgsListSecrets(&o.Factory),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return o.Validate(args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.Run(cmd.Context(), args, ioStreams.Out)
		},
	}

	o.Factory = factory.New(cmd)

	return cmd
}

// Validate validates the provided options
func (o *Options) Validate(args []string) error {
	if len(args) < 1 {
		return errors.New("the name of the Secret has to be provided as argument")
	}
	if len(args) > 1 {
		return errors.New("only one argument can be passed in: the name of the Secret")
	}
	return nil
}

// Run executes status certificate command
func (o *Options) Run(ctx context.Context, args []string, stdout io.Writer) error {
	secret, err := o.KubeClient.CoreV1().Secrets(o.Namespace).Get(ctx, args[0], metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error when finding Secret %q: %w\n", args[0], err)
	}

	certData := secret.Data[corev1.TLSCertKey]
	certs, err := splitPEMs(certData)
	if err != nil {
		return err
	}
	if len(certs) < 1 {
		return errors.New("no PEM data found in secret")
	}

	intermediates := [][]byte(nil)
	if len(certs) > 1 {
		intermediates = certs[1:]
	}

	// we only want to inspect the leaf certificate
	x509Cert, err := pki.DecodeX509CertificateBytes(certs[0])
	if err != nil {
		return fmt.Errorf("error when parsing 'tls.crt': %w", err)
	}

	var out []string

	for _, describeFn := range []func(*x509.Certificate) (string, error){
		describeValidFor,
		describeValidityPeriod,
		describeIssuedBy,
		describeIssuedFor,
		describeCertificate,
	} {
		desc, err := describeFn(x509Cert)
		if err != nil {
			return err
		}
		out = append(out, desc)
	}

	if desc, err := describeDebugging(ctx, x509Cert, intermediates, secret.Data[cmmeta.TLSCAKey]); err != nil {
		return err
	} else {
		out = append(out, desc)
	}

	fmt.Fprintln(stdout, strings.Join(out, "\n\n"))

	return nil
}

func describeValidFor(cert *x509.Certificate) (string, error) {
	tmpl, err := template.New("validForTemplate").Parse(validForTemplate)
	if err != nil {
		return "", err
	}

	var b bytes.Buffer
	err = tmpl.Execute(&b, struct {
		DNSNames       string
		URIs           string
		IPAddresses    string
		EmailAddresses string
		KeyUsage       string
	}{
		DNSNames:       printSlice(cert.DNSNames),
		URIs:           printSlice(pki.URLsToString(cert.URIs)),
		IPAddresses:    printSlice(pki.IPAddressesToString(cert.IPAddresses)),
		EmailAddresses: printSlice(cert.EmailAddresses),
		KeyUsage:       printKeyUsage(pki.BuildCertManagerKeyUsages(cert.KeyUsage, cert.ExtKeyUsage)),
	})

	return b.String(), err
}

func describeValidityPeriod(cert *x509.Certificate) (string, error) {
	tmpl, err := template.New("validityPeriodTemplate").Parse(validityPeriodTemplate)
	if err != nil {
		return "", err
	}

	var b bytes.Buffer
	err = tmpl.Execute(&b, struct {
		NotBefore string
		NotAfter  string
	}{
		NotBefore: cert.NotBefore.Format(time.RFC1123),
		NotAfter:  cert.NotAfter.Format(time.RFC1123),
	})

	return b.String(), err
}

func describeIssuedBy(cert *x509.Certificate) (string, error) {
	tmpl, err := template.New("issuedByTemplate").Parse(issuedByTemplate)
	if err != nil {
		return "", err
	}

	var b bytes.Buffer
	err = tmpl.Execute(&b, struct {
		CommonName         string
		Organization       string
		OrganizationalUnit string
		Country            string
	}{
		CommonName:         printOrNone(cert.Issuer.CommonName),
		Organization:       printSliceOrOne(cert.Issuer.Organization),
		OrganizationalUnit: printSliceOrOne(cert.Issuer.OrganizationalUnit),
		Country:            printSliceOrOne(cert.Issuer.Country),
	})

	return b.String(), err
}

func describeIssuedFor(cert *x509.Certificate) (string, error) {
	tmpl, err := template.New("issuedForTemplate").Parse(issuedForTemplate)
	if err != nil {
		return "", err
	}

	var b bytes.Buffer
	err = tmpl.Execute(&b, struct {
		CommonName         string
		Organization       string
		OrganizationalUnit string
		Country            string
	}{
		CommonName:         printOrNone(cert.Subject.CommonName),
		Organization:       printSliceOrOne(cert.Subject.Organization),
		OrganizationalUnit: printSliceOrOne(cert.Subject.OrganizationalUnit),
		Country:            printSliceOrOne(cert.Subject.Country),
	})

	return b.String(), err
}

func describeCertificate(cert *x509.Certificate) (string, error) {
	tmpl, err := template.New("certificateTemplate").Parse(certificateTemplate)
	if err != nil {
		return "", err
	}

	var b bytes.Buffer
	err = tmpl.Execute(&b, struct {
		SigningAlgorithm   string
		PublicKeyAlgorithm string
		SerialNumber       string
		Fingerprints       string
		IsCACertificate    bool
		CRL                string
		OCSP               string
	}{
		SigningAlgorithm:   cert.SignatureAlgorithm.String(),
		PublicKeyAlgorithm: cert.PublicKeyAlgorithm.String(),
		SerialNumber:       cert.SerialNumber.String(),
		Fingerprints:       fingerprintCert(cert),
		IsCACertificate:    cert.IsCA,
		CRL:                printSliceOrOne(cert.CRLDistributionPoints),
		OCSP:               printSliceOrOne(cert.OCSPServer),
	})

	return b.String(), err
}

func describeDebugging(ctx context.Context, cert *x509.Certificate, intermediates [][]byte, ca []byte) (string, error) {
	tmpl, err := template.New("debuggingTemplate").Parse(debuggingTemplate)
	if err != nil {
		return "", err
	}

	var b bytes.Buffer
	err = tmpl.Execute(&b, struct {
		TrustedByThisComputer string
		CRLStatus             string
		OCSPStatus            string
	}{
		TrustedByThisComputer: describeTrusted(cert, intermediates),
		CRLStatus:             describeCRL(ctx, cert),
		OCSPStatus:            describeOCSP(ctx, cert, intermediates, ca),
	})

	return b.String(), err
}

func describeCRL(ctx context.Context, cert *x509.Certificate) string {
	if len(cert.CRLDistributionPoints) < 1 {
		return "No CRL endpoints set"
	}

	hasChecked := false
	for _, crlURL := range cert.CRLDistributionPoints {
		u, err := url.Parse(crlURL)
		if err != nil {
			return fmt.Sprintf("Invalid CRL URL: %v", err)
		}
		if u.Scheme != "ldap" && u.Scheme != "https" {
			continue
		}

		hasChecked = true
		valid, err := checkCRLValidCert(ctx, cert, crlURL)
		if err != nil {
			return fmt.Sprintf("Cannot check CRL: %s", err.Error())
		}
		if !valid {
			return fmt.Sprintf("Revoked by %s", crlURL)
		}
	}

	if !hasChecked {
		return "No CRL endpoints we support found"
	}

	return "Valid"
}

func describeOCSP(ctx context.Context, cert *x509.Certificate, intermediates [][]byte, ca []byte) string {
	if len(ca) > 1 {
		intermediates = append([][]byte{ca}, intermediates...)
	}
	if len(intermediates) < 1 {
		return "Cannot check OCSP, does not have a CA or intermediate certificate provided"
	}
	issuerCert, err := pki.DecodeX509CertificateBytes(intermediates[len(intermediates)-1])
	if err != nil {
		return fmt.Sprintf("Cannot parse intermediate certificate: %s", err.Error())
	}

	valid, err := checkOCSPValidCert(ctx, cert, issuerCert)
	if err != nil {
		return fmt.Sprintf("Cannot check OCSP: %s", err.Error())
	}

	if !valid {
		return "Marked as revoked"
	}

	return "valid"
}

func describeTrusted(cert *x509.Certificate, intermediates [][]byte) string {
	systemPool, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Sprintf("Error getting system CA store: %s", err.Error())
	}
	for _, intermediate := range intermediates {
		systemPool.AppendCertsFromPEM(intermediate)
	}
	_, err = cert.Verify(x509.VerifyOptions{
		Roots:       systemPool,
		CurrentTime: clock.Now(),
	})
	if err == nil {
		return "yes"
	}
	return fmt.Sprintf("no: %s", err.Error())
}
